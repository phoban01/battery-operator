/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package execagenttest is the test environment of the Exec Agent
// (docs/requirements/08-test-doubles.md#test-environments): a real
// kube-apiserver and etcd, started by controller-runtime's envtest, serving
// the provisional test definition of the claim resource, with the agent's
// shipped RBAC and admission policy applied from config/exec-agent; and,
// per Host, a Node, a fake flintlockd (internal/fakeflintlock) served over
// mutual TLS on loopback and a real Exec Agent in front of it,
// authenticating with a real bound ServiceAccount token of an Exec Agent pod
// on that Node. There is no kubelet, no KVM and no battery: a test plays
// battery by writing claims itself, and plays the guest by scripting the
// fake flintlockd's exec.
package execagenttest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/liquidmetal-dev/flintlock/api/types"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcheck"
)

const (
	// AssetsVar names the directory of kube-apiserver and etcd, which
	// `make test` sets from setup-envtest.
	AssetsVar = "KUBEBUILDER_ASSETS"
	// AgentNamespace is the namespace of config/exec-agent, where the guard
	// pods live.
	AgentNamespace = "battery-operator-system"
	// AgentServiceAccount is the Exec Agent's ServiceAccount.
	AgentServiceAccount = "battery-operator-exec-agent"
	// AgentUser is the user name of AgentServiceAccount.
	AgentUser = "system:serviceaccount:" + AgentNamespace + ":" + AgentServiceAccount
	// HostAddress is the internal address of every Host's Node here, which
	// is where each Exec Agent listens, where each fake flintlockd listens,
	// and what the test certificates name.
	HostAddress = "127.0.0.1"
	// WaitTimeout bounds every wait of the environment.
	WaitTimeout = 30 * time.Second
)

// ErrNoAssets is returned by Start when AssetsVar is unset: the caller
// skips. `make test` always sets it.
var ErrNoAssets = errors.New("execagenttest: no API server test environment: run `make test`, or set " + AssetsVar)

// Env is one running API server.
type Env struct {
	env *envtest.Environment
	// Config is the administrator's configuration.
	Config *rest.Config
	// Admin is the test's own client: it plays battery, the Host's kubelet
	// and the operator.
	Admin kubernetes.Interface
	// Dynamic is the administrator's client of the claim resource.
	Dynamic dynamic.Interface
	// Certs are one CA and what it signed: the serving certificate of every
	// Exec Agent and every fake flintlockd, for HostAddress, and the client
	// certificate each Exec Agent presents to its flintlockd.
	Certs *fakeflintlock.TestCerts

	dir string
	seq atomic.Int64
}

// ModuleRoot is the directory of go.mod: this file is three directories
// below it.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

//= docs/requirements/08-test-doubles.md#test-environments
//# The Exec Agent SHALL be tested against envtest serving the
//# CRDs, and the fake `flintlockd`.

// Start starts kube-apiserver and etcd with the claim resource's test
// definition installed, config/exec-agent/rbac.yaml and
// config/exec-agent/admission-policy.yaml applied unchanged, and waits
// until the API server enforces the policy.
func Start() (*Env, error) {
	if os.Getenv(AssetsVar) == "" {
		return nil, ErrNoAssets
	}
	root := ModuleRoot()
	dir, err := os.MkdirTemp("", "execagenttest-")
	if err != nil {
		return nil, err
	}
	certs, err := fakeflintlock.WriteTestCerts(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "internal", "execagent", "testdata", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("execagenttest: starting the API server test environment: %w", err)
	}
	e := &Env{env: env, Config: cfg, Certs: certs, dir: dir}
	ctx := context.Background()
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		e.Dynamic, err = dynamic.NewForConfig(cfg)
	}
	if err == nil {
		_, err = e.Admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: AgentNamespace}}, metav1.CreateOptions{})
	}
	if err == nil {
		_, err = e.Admin.CoreV1().ServiceAccounts(AgentNamespace).Create(ctx,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: AgentServiceAccount}}, metav1.CreateOptions{})
	}
	for _, manifest := range []string{"rbac.yaml", "admission-policy.yaml"} {
		if err == nil {
			err = e.Apply(ctx, filepath.Join(root, "config", "exec-agent", manifest))
		}
	}
	if err == nil {
		err = e.awaitPolicy(ctx)
	}
	if err != nil {
		_ = env.Stop()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return e, nil
}

// Stop stops the API server and etcd and removes the certificates.
func (e *Env) Stop() error {
	err := e.env.Stop()
	_ = os.RemoveAll(e.dir)
	return err
}

// Apply creates every object of a manifest as the administrator.
func (e *Env) Apply(ctx context.Context, manifest string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(e.Config)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))
	for doc := range strings.SplitSeq(string(data), "\n---") {
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return fmt.Errorf("%s: %w", manifest, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		gvk := obj.GroupVersionKind()
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", manifest, gvk.Kind, err)
		}
		var client dynamic.ResourceInterface = e.Dynamic.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			client = e.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		}
		if _, err := client.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("%s: %s %s: %w", manifest, gvk.Kind, obj.GetName(), err)
		}
	}
	return nil
}

// awaitPolicy waits until the admission policy refuses what it has to: an
// Exec Agent labelling a Node, tried as a dry run.
func (e *Env) awaitPolicy(ctx context.Context) error {
	cfg := rest.CopyConfig(e.Config)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: AgentUser,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + AgentNamespace, "system:authenticated"},
		Extra:    map[string][]string{execagent.NodeNameExtra: {"execagenttest-policy-probe"}},
	}
	probe, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	if _, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "execagenttest-policy-probe"}}, metav1.CreateOptions{}); err != nil {
		return err
	}
	deadline := time.Now().Add(WaitTimeout)
	for {
		_, err := probe.CoreV1().Nodes().Patch(ctx, "execagenttest-policy-probe", k8stypes.MergePatchType,
			[]byte(`{"metadata":{"labels":{"probe":"x"}}}`), metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
		if IsPolicyDenial(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("execagenttest: the admission policy was not enforced within %s: the probe got %w", WaitTimeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// IsPolicyDenial reports whether err is a refusal by a
// ValidatingAdmissionPolicy, as opposed to one by RBAC or anything else.
func IsPolicyDenial(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy")
}

// Namespace creates a namespace of its own for a test, standing for a
// consumer's.
func (e *Env) Namespace(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("consumer-%d", e.seq.Add(1))
	if _, err := e.Admin.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	return name
}

// Identity is a ServiceAccount with a real token of its own.
type Identity struct {
	// User is the user name the API server authenticates the token as.
	User string
	// Token is the token.
	Token string
}

// ServiceAccountToken creates a ServiceAccount and requests a token for
// it, standing for a consumer's projected token. The token is bound to no
// object; a TokenReview authenticates it all the same.
func (e *Env) ServiceAccountToken(t testing.TB, namespace, name string) Identity {
	t.Helper()
	ctx := context.Background()
	_, err := e.Admin.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating service account %s/%s: %v", namespace, name, err)
	}
	return Identity{User: "system:serviceaccount:" + namespace + ":" + name, Token: e.token(t, namespace, name, nil)}
}

// token requests a token of a ServiceAccount, bound to the object when one
// is given.
func (e *Env) token(t testing.TB, namespace, name string, bound *authenticationv1.BoundObjectReference) string {
	t.Helper()
	expiry := int64(3600)
	tok, err := e.Admin.CoreV1().ServiceAccounts(namespace).CreateToken(context.Background(), name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiry, BoundObjectRef: bound}},
		metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token of %s/%s: %v", namespace, name, err)
	}
	return tok.Status.Token
}

// AgentConfig is the client configuration of the Exec Agent of the Host
// whose Node is hostNode: the bound ServiceAccount token of an Exec Agent
// pod on that Node, which is what the kubelet projects into the pod and
// what the agent authenticates with in-cluster, and nothing else.
func (e *Env) AgentConfig(t testing.TB, hostNode string) *rest.Config {
	t.Helper()
	ctx := context.Background()
	pod, err := e.Admin.CoreV1().Pods(AgentNamespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "exec-agent-", Namespace: AgentNamespace},
		Spec: corev1.PodSpec{
			NodeName:           hostNode,
			ServiceAccountName: AgentServiceAccount,
			Containers:         []corev1.Container{{Name: "exec-agent", Image: "exec-agent"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the exec agent pod on %s: %v", hostNode, err)
	}
	token := e.token(t, AgentNamespace, AgentServiceAccount,
		&authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID})
	cfg := rest.AnonymousClientConfig(e.Config)
	cfg.BearerToken = token
	return cfg
}

// ClaimStatus is what battery would write into a claim's status.
type ClaimStatus struct {
	Phase     execagent.ClaimPhase
	VMUID     string
	HostNode  string
	ExpiresAt time.Time
}

// PutClaim creates or replaces a claim of the provisional resource,
// recording creator as the identity that created it, and writes its
// status as battery would.
func (e *Env) PutClaim(t testing.TB, namespace, name, creator string, st ClaimStatus) {
	t.Helper()
	ctx := context.Background()
	res := execagent.ProvisionalClaimResource
	client := e.Dynamic.Resource(res.GroupVersionResource()).Namespace(namespace)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": res.Group + "/" + res.Version,
		"kind":       "MicroVMClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{"poolRef": map[string]any{"name": "pool"}},
	}}
	if creator != "" {
		obj.SetAnnotations(map[string]string{res.CreatorAnnotation: creator})
	}
	existing, err := client.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		existing, err = client.Create(ctx, obj, metav1.CreateOptions{})
	case err == nil:
		obj.SetResourceVersion(existing.GetResourceVersion())
		existing, err = client.Update(ctx, obj, metav1.UpdateOptions{})
	}
	if err != nil {
		t.Fatalf("writing claim %s/%s: %v", namespace, name, err)
	}
	status := map[string]any{}
	if st.Phase != "" {
		status["phase"] = string(st.Phase)
	}
	if st.VMUID != "" {
		status["microVM"] = map[string]any{"uid": st.VMUID}
	}
	if st.HostNode != "" {
		status["host"] = map[string]any{"nodeName": st.HostNode}
	}
	if !st.ExpiresAt.IsZero() {
		status["leaseExpiresAt"] = st.ExpiresAt.UTC().Format(time.RFC3339)
	}
	existing.Object["status"] = status
	if _, err := client.UpdateStatus(ctx, existing, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("writing the status of claim %s/%s: %v", namespace, name, err)
	}
}

// DeleteClaim deletes a claim, as its consumer does when it is done.
func (e *Env) DeleteClaim(t testing.TB, namespace, name string) {
	t.Helper()
	err := e.Dynamic.Resource(execagent.ProvisionalClaimResource.GroupVersionResource()).Namespace(namespace).
		Delete(context.Background(), name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting claim %s/%s: %v", namespace, name, err)
	}
}

// HostOptions shape one Host.
type HostOptions struct {
	// ExecOpenTimeout is the agent's deadline for flintlockd to open an
	// exec stream (EA-021); zero is two seconds.
	ExecOpenTimeout time.Duration
	// DrainTimeout is the agent's drain timeout (EA-040); zero is an hour.
	DrainTimeout time.Duration
	// ExecDisabled makes the fake flintlockd report exec disabled.
	ExecDisabled bool
}

// Host is one Host: its Node, its fake flintlockd, one CREATED MicroVM on
// it, and its Exec Agent.
type Host struct {
	env  *Env
	t    testing.TB
	opts HostOptions
	// Node is the name of the Host's Node.
	Node string
	// Fake is the fake flintlockd.
	Fake *fakeflintlock.Server
	// VMUID is the uid of the MicroVM created on the Host.
	VMUID string
	// NotReadyDir is the agent's not ready reason directory.
	NotReadyDir string
	// KVMDevice is the file the agent opens as the KVM device; removing it
	// makes KVM unavailable (EA-031).
	KVMDevice string
	// SysBlockDir is the agent's fake sysfs block directory, holding
	// containerd's thin pool under its default name (EA-032).
	SysBlockDir string
	// Address is where the Exec Agent serves, HostAddress and its port.
	Address string

	logs *syncBuffer

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
}

// NewHost creates a Host's Node at HostAddress, a fake flintlockd served on
// loopback over mutual TLS with one CREATED MicroVM, and starts the Exec
// Agent in front of it with its own bound identity, a real claim lookup and
// the shipped RBAC and admission policy. Everything stops when the test
// ends.
func (e *Env) NewHost(t testing.TB, opts HostOptions) *Host {
	t.Helper()
	ctx := context.Background()
	id := e.seq.Add(1)
	h := &Host{env: e, t: t, opts: opts, Node: fmt.Sprintf("host-%d", id), logs: &syncBuffer{}}

	node, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.Node}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host's node: %v", err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: HostAddress}}
	if _, err := e.Admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setting the host's node status: %v", err)
	}

	h.Fake = fakeflintlock.New(fakeflintlock.Config{
		Name: h.Node, Listen: net.JoinHostPort(HostAddress, "0"), Version: "test",
		ExecEnabled: !opts.ExecDisabled, TLS: e.Certs.ServerTLS(true),
	})
	serveCtx, stopServe := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- h.Fake.Serve(serveCtx) }()
	t.Cleanup(func() {
		stopServe()
		<-served
	})
	select {
	case <-h.Fake.Ready():
	case err := <-served:
		t.Fatalf("the fake flintlockd did not start: %v", err)
	}
	vm, err := h.Fake.CreateMicroVM(&types.MicroVMSpec{Id: "vm", Namespace: "execagenttest"})
	if err != nil {
		t.Fatalf("creating a microvm: %v", err)
	}
	h.VMUID = vm.GetSpec().GetUid()
	h.NotReadyDir = filepath.Join(t.TempDir(), "not-ready.d")
	h.KVMDevice, h.SysBlockDir = FakeHostPrerequisites(t)

	listener, err := net.Listen("tcp", net.JoinHostPort(HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	h.Address = listener.Addr().String()
	h.start(listener)
	t.Cleanup(func() {
		h.StopAgent()
		if t.Failed() {
			t.Logf("exec agent log of %s:\n%s", h.Node, h.logs.String())
		}
	})
	return h
}

// FakeHostPrerequisites makes stand-ins for the Host prerequisites the
// agent checks on the Host itself, since there is no KVM here: a file that
// opens in place of /dev/kvm, and a sysfs block directory whose one
// device-mapper device, dm-0, is containerd's thin pool under its default
// name. It returns the KVM device and the block directory.
func FakeHostPrerequisites(t testing.TB) (kvmDevice, sysBlockDir string) {
	t.Helper()
	dir := t.TempDir()
	kvmDevice = filepath.Join(dir, "kvm")
	if err := os.WriteFile(kvmDevice, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sysBlockDir = filepath.Join(dir, "sys", "block")
	dm := filepath.Join(sysBlockDir, "dm-0", "dm")
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dm, "name"), []byte(hostcheck.DefaultThinPool+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return kvmDevice, sysBlockDir
}

// Log is what the Exec Agent has logged.
func (h *Host) Log() string { return h.logs.String() }

// Config is the agent's configuration on this Host.
func (h *Host) Config() *execagent.Config {
	open := h.opts.ExecOpenTimeout
	if open == 0 {
		open = 2 * time.Second
	}
	certs := h.env.Certs
	cfg := &execagent.Config{
		HostNode:   h.Node,
		Flintlockd: h.Fake.Addr(),
		FlintlockdTLS: execagent.ClientTLS{
			CertFile: certs.ClientCertFile, KeyFile: certs.ClientKeyFile, CAFile: certs.CAFile,
		},
		TLS:             execagent.ServerTLS{CertFile: certs.ServerCertFile, KeyFile: certs.ServerKeyFile},
		ExecOpenTimeout: open,
		CallTimeout:     5 * time.Second,
		NotReadyDir:     h.NotReadyDir,
		KVMDevice:       h.KVMDevice,
		SysBlockDir:     h.SysBlockDir,
		DrainTimeout:    h.opts.DrainTimeout,
		Guard:           execagent.Guard{Namespace: AgentNamespace},
		SyncInterval:    50 * time.Millisecond,
	}
	cfg.ApplyDefaults()
	return cfg
}

// start runs the Exec Agent on listener: the shipped RBAC, under the
// shipped admission policy, with an identity that names its Host.
func (h *Host) start(listener net.Listener) {
	h.t.Helper()
	cfg := h.Config()
	restConfig := h.env.AgentConfig(h.t, h.Node)
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		h.t.Fatal(err)
	}
	fl, err := execagent.DialFlintlockd(cfg.Flintlockd, cfg.FlintlockdTLS, []net.IP{net.ParseIP(HostAddress)})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	claims := execagent.NewDynamicClaims(dyn, execagent.ProvisionalClaimResource, time.Minute)
	go claims.Run(ctx)

	logger := funcr.New(func(prefix, args string) {
		_, _ = fmt.Fprintln(h.logs, prefix, args)
	}, funcr.Options{Verbosity: 1})
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer func() { _ = fl.Close() }()
		done <- execagent.Run(ctx, execagent.Options{
			Config: cfg, Kube: kube, Claims: claims, Flintlockd: fl, Listener: listener, Ready: ready,
			HostAddresses: []net.IP{net.ParseIP(HostAddress)}, Logger: logger,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		h.t.Fatalf("the exec agent stopped before it was ready: %v\n%s", err, h.logs.String())
	case <-time.After(WaitTimeout):
		cancel()
		h.t.Fatalf("the exec agent was not ready in %s\n%s", WaitTimeout, h.logs.String())
	}
	deadline := time.Now().Add(WaitTimeout)
	for !claims.HasSynced() {
		if time.Now().After(deadline) {
			cancel()
			h.t.Fatalf("the exec agent's claims were not listed in %s", WaitTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	h.cancel, h.done = cancel, done
	h.mu.Unlock()
}

// StopAgent stops the Exec Agent, as a restart of its container does. Its
// in-flight streams are cut after the agent's stop grace.
func (h *Host) StopAgent() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(WaitTimeout):
		h.t.Errorf("the exec agent of %s did not stop in %s", h.Node, WaitTimeout)
	}
}

// StartAgent starts the Exec Agent again on the same address, as the
// restarted container does.
func (h *Host) StartAgent() {
	h.t.Helper()
	var listener net.Listener
	var err error
	deadline := time.Now().Add(WaitTimeout)
	for {
		if listener, err = net.Listen("tcp", h.Address); err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("listening on %s again: %v", h.Address, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.start(listener)
}

// ReadNode reads the Host's Node.
func (h *Host) ReadNode() *corev1.Node {
	h.t.Helper()
	node, err := h.env.Admin.CoreV1().Nodes().Get(context.Background(), h.Node, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("reading the host's node: %v", err)
	}
	return node
}

// Eventually polls ok until it holds or WaitTimeout passes.
func Eventually(t testing.TB, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", WaitTimeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncBuffer is a log buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
