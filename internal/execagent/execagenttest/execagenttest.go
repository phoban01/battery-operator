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
// this project's CRDs from config/crd/bases, with the agent's
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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/liquidmetal-dev/flintlock/api/types"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller"
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
	// TrustDomain is the SPIFFE trust domain of the Operator's signer and
	// of every Exec Agent.
	TrustDomain = "execagenttest.example"
	// CertificateMaxDuration is the longest validity the signer issues.
	CertificateMaxDuration = time.Hour
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
	// Dynamic is the administrator's client of any resource, which Apply
	// uses.
	Dynamic dynamic.Interface
	// Claims is the administrator's typed client of MicroVMClaims.
	Claims client.Client
	// ServingCAFile is the serving CA's certificate, which signs every Exec
	// Agent's and every fake flintlockd's serving certificate: what a
	// consumer verifies an Exec Agent against.
	ServingCAFile string

	dir        string
	seq        atomic.Int64
	stopSigner context.CancelFunc
	signerDone chan error
}

// ModuleRoot is the directory of go.mod: this file is three directories
// below it.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// Start starts kube-apiserver and etcd with this project's CRDs
// installed, config/exec-agent/rbac.yaml and
// config/exec-agent/admission-policy.yaml applied unchanged, and the
// Operator's CertificateSigningRequest signer (internal/controller) running
// in-process with throwaway CAs, and waits until the API server enforces
// the policy and the signer has published its CA certificates.
func Start() (*Env, error) {
	if os.Getenv(AssetsVar) == "" {
		return nil, ErrNoAssets
	}
	root := ModuleRoot()
	dir, err := os.MkdirTemp("", "execagenttest-")
	if err != nil {
		return nil, err
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("execagenttest: starting the API server test environment: %w", err)
	}
	e := &Env{env: env, Config: cfg, dir: dir}
	ctx := context.Background()
	if e.Admin, err = kubernetes.NewForConfig(cfg); err == nil {
		e.Dynamic, err = dynamic.NewForConfig(cfg)
	}
	if err == nil {
		e.Claims, err = client.New(cfg, client.Options{Scheme: scheme()})
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
	if err == nil {
		err = e.startSigner(ctx)
	}
	if err != nil {
		e.stopSignerAndWait()
		_ = env.Stop()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return e, nil
}

// Stop stops the signer, the API server and etcd, and removes the
// certificates.
func (e *Env) Stop() error {
	e.stopSignerAndWait()
	err := e.env.Stop()
	_ = os.RemoveAll(e.dir)
	return err
}

// startSigner creates a throwaway serving CA and client CA as the CA
// Secrets, starts the Operator's CertificateSigningRequest signer on them,
// approving requests from AgentServiceAccount, and waits until it has
// published the CA certificates.
func (e *Env) startSigner(ctx context.Context) error {
	servingCA, err := newCA("execagenttest serving CA")
	if err != nil {
		return err
	}
	clientCA, err := newCA("execagenttest flintlockd client CA")
	if err != nil {
		return err
	}
	e.ServingCAFile = filepath.Join(e.dir, "serving-ca.crt")
	if err := os.WriteFile(e.ServingCAFile, servingCA.certPEM, 0o600); err != nil {
		return err
	}
	for name, ca := range map[string]*testCA{
		controller.DefaultServingCASecret: servingCA,
		controller.DefaultClientCASecret:  clientCA,
	} {
		if _, err := e.Admin.CoreV1().Secrets(AgentNamespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: AgentNamespace, Name: name},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: ca.certPEM, corev1.TLSPrivateKeyKey: ca.keyPEM},
		}, metav1.CreateOptions{}); err != nil {
			return err
		}
	}

	ctrllog.SetLogger(logr.Discard())
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(e.Config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return err
	}
	signerConfig := controller.SignerConfig{
		TrustDomain:             TrustDomain,
		MaxDuration:             CertificateMaxDuration,
		Namespace:               AgentNamespace,
		ServingCASecret:         controller.DefaultServingCASecret,
		ClientCASecret:          controller.DefaultClientCASecret,
		CABundleConfigMap:       controller.DefaultCABundleConfigMap,
		ExecAgentServiceAccount: AgentServiceAccount,
	}
	if err := signerConfig.Complete(); err != nil {
		return err
	}
	if err := (&controller.CertificateSigningRequestReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Config: signerConfig,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	signerCtx, stop := context.WithCancel(context.Background())
	e.stopSigner, e.signerDone = stop, make(chan error, 1)
	go func() { e.signerDone <- mgr.Start(signerCtx) }()

	deadline := time.Now().Add(WaitTimeout)
	for {
		cm, err := e.Admin.CoreV1().ConfigMaps(AgentNamespace).Get(ctx, controller.DefaultCABundleConfigMap, metav1.GetOptions{})
		if err == nil && cm.Data[controller.ServingCAKey] != "" && cm.Data[controller.ClientCAKey] != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("execagenttest: the signer did not publish its CA certificates within %s: %v", WaitTimeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stopSignerAndWait stops the signer, if it runs.
func (e *Env) stopSignerAndWait() {
	if e.stopSigner == nil {
		return
	}
	e.stopSigner()
	<-e.signerDone
	e.stopSigner = nil
}

// testCA is a throwaway CA in the layout cert-manager gives a CA's Secret.
type testCA struct {
	certPEM []byte
	keyPEM  []byte
}

func newCA(cn string) (*testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &testCA{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
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
		var res dynamic.ResourceInterface = e.Dynamic.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			res = e.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		}
		if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
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
	// Namespace and Name are the ServiceAccount's.
	Namespace string
	Name      string
	// User is the user name the API server authenticates the token as.
	User string
	// Token is the token.
	Token string
}

// ServiceAccountToken creates a ServiceAccount and requests a token for
// it, standing for a consumer's projected token: for the API server's own
// audience and bound to no object. The Exec Agent refuses it (EA-010);
// Token requests one for the agent's audience.
func (e *Env) ServiceAccountToken(t testing.TB, namespace, name string) Identity {
	t.Helper()
	_, err := e.Admin.CoreV1().ServiceAccounts(namespace).Create(context.Background(),
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating service account %s/%s: %v", namespace, name, err)
	}
	return Identity{
		Namespace: namespace, Name: name, User: "system:serviceaccount:" + namespace + ":" + name,
		Token: e.Token(t, namespace, name, nil, nil),
	}
}

// ClaimToken requests a claim token as the Client Library does (CC-002):
// a token of the ServiceAccount namespace/serviceAccount, bound to the
// Secret secretName in that namespace by its name and uid, with the Exec
// Agent's audience.
func (e *Env) ClaimToken(t testing.TB, namespace, serviceAccount, secretName string) string {
	t.Helper()
	secret, err := e.Admin.CoreV1().Secrets(namespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret %s/%s: %v", namespace, secretName, err)
	}
	return e.Token(t, namespace, serviceAccount, []string{execagent.DefaultTokenAudience},
		&authenticationv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: secret.Name, UID: secret.UID})
}

// Token requests a token of a ServiceAccount with TokenRequest, for the
// audiences given or the API server's own when there are none, and bound
// to the object when one is given.
func (e *Env) Token(t testing.TB, namespace, name string, audiences []string, bound *authenticationv1.BoundObjectReference) string {
	t.Helper()
	expiry := int64(3600)
	tok, err := e.Admin.CoreV1().ServiceAccounts(namespace).CreateToken(context.Background(), name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences: audiences, ExpirationSeconds: &expiry, BoundObjectRef: bound,
		}},
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
	token := e.Token(t, AgentNamespace, AgentServiceAccount, nil,
		&authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID})
	cfg := rest.AnonymousClientConfig(e.Config)
	cfg.BearerToken = token
	return cfg
}

// ClaimStatus is what the Claim Controller would write into a claim's
// status from battery's answer.
type ClaimStatus struct {
	Phase     batteryv1alpha1.MicroVMClaimPhase
	VMUID     string
	HostNode  string
	ExpiresAt time.Time
}

// PutClaim creates claim namespace/name for the Holder serviceAccount, or
// replaces its status when it exists, and writes its status as the Claim
// Controller would. It also creates the claim's Secret `<name>-exec`,
// owned by the claim, as the Client Library does (CC-001), unless it
// exists.
func (e *Env) PutClaim(t testing.TB, namespace, name, serviceAccount string, st ClaimStatus) {
	t.Helper()
	ctx := context.Background()
	claim := &batteryv1alpha1.MicroVMClaim{}
	err := e.Claims.Get(ctx, k8stypes.NamespacedName{Namespace: namespace, Name: name}, claim)
	switch {
	case apierrors.IsNotFound(err):
		claim = &batteryv1alpha1.MicroVMClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: batteryv1alpha1.MicroVMClaimSpec{
				PoolRef:            batteryv1alpha1.PoolReference{Name: "pool"},
				ServiceAccountName: serviceAccount,
			},
		}
		err = e.Claims.Create(ctx, claim)
	case err == nil && claim.Spec.ServiceAccountName != serviceAccount:
		err = fmt.Errorf("the claim exists for %q; its holder cannot change", claim.Spec.ServiceAccountName)
	}
	if err != nil {
		t.Fatalf("writing claim %s/%s: %v", namespace, name, err)
	}
	claim.Status = batteryv1alpha1.MicroVMClaimStatus{Phase: st.Phase}
	if st.VMUID != "" {
		claim.Status.MicroVM = &batteryv1alpha1.MicroVMReference{UID: st.VMUID}
	}
	if st.HostNode != "" {
		claim.Status.Host = &batteryv1alpha1.HostReference{NodeName: st.HostNode}
	}
	if !st.ExpiresAt.IsZero() {
		claim.Status.LeaseExpiresAt = &metav1.Time{Time: st.ExpiresAt}
	}
	if err := e.Claims.Status().Update(ctx, claim); err != nil {
		t.Fatalf("writing the status of claim %s/%s: %v", namespace, name, err)
	}
	_, err = e.Admin.CoreV1().Secrets(namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name + execagent.ExecSecretSuffix,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batteryv1alpha1.GroupVersion.String(), Kind: "MicroVMClaim", Name: claim.Name, UID: claim.UID,
		}},
	}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating the secret of claim %s/%s: %v", namespace, name, err)
	}
}

// DeleteClaim deletes a claim, as its consumer does when it is done. There
// is no garbage collector here, so its Secret stays, as it does for the
// few seconds a real one takes.
func (e *Env) DeleteClaim(t testing.TB, namespace, name string) {
	t.Helper()
	err := e.Claims.Delete(context.Background(), &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting claim %s/%s: %v", namespace, name, err)
	}
}

// scheme is the scheme of the typed claim client.
func scheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	utilruntime.Must(batteryv1alpha1.AddToScheme(s))
	return s
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
	// CertificateClock, when set, schedules the agent's certificate
	// renewals; nil is the real clock.
	CertificateClock clock.Clock
	// FlintlockdTLS, when set, is what the fake flintlockd serves with in
	// place of the certificate the agent writes for it.
	FlintlockdTLS *fakeflintlock.TLS
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
	// KVMDevice is the KVM device node the agent checks, /dev/null here.
	KVMDevice string
	// KVMSysfsDir is the fake sysfs directory listing the KVM device;
	// removing its dev file makes KVM unavailable (EA-031).
	KVMSysfsDir string
	// SysBlockDir is the agent's fake sysfs block directory, holding
	// containerd's thin pool under its default name (EA-032).
	SysBlockDir string
	// Address is where the Exec Agent serves, HostAddress and its port.
	Address string
	// CertDir is the Host directory the agent writes flintlockd's
	// certificate, key and client CA bundle to.
	CertDir string

	logs *syncBuffer
	// flintlockd is the fake flintlockd's endpoint.
	flintlockd string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan error
	certs  *execagent.Certificates
}

// NewHost creates a Host's Node at HostAddress and starts its Exec Agent
// with its own bound identity, a real claim lookup and the shipped RBAC and
// admission policy. Once the agent has obtained its certificates from the
// signer, it starts a fake flintlockd on loopback with the files the agent
// wrote, over mutual TLS, with one CREATED MicroVM, as the Host Image
// starts flintlockd once they exist. Everything stops when the test ends.
func (e *Env) NewHost(t testing.TB, opts HostOptions) *Host {
	t.Helper()
	ctx := context.Background()
	id := e.seq.Add(1)
	h := &Host{env: e, t: t, opts: opts, Node: fmt.Sprintf("host-%d", id), logs: &syncBuffer{}, CertDir: filepath.Join(t.TempDir(), "flintlockd")}

	node, err := e.Admin.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: h.Node}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the host's node: %v", err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: HostAddress}}
	if _, err := e.Admin.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("setting the host's node status: %v", err)
	}

	// flintlockd's port is chosen now, since the agent is configured with
	// it, and flintlockd starts only after the agent.
	probe, err := net.Listen("tcp", net.JoinHostPort(HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	h.flintlockd = probe.Addr().String()
	_ = probe.Close()
	h.NotReadyDir = filepath.Join(t.TempDir(), "not-ready.d")
	h.KVMDevice, h.KVMSysfsDir, h.SysBlockDir = FakeHostPrerequisites(t)

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

	serverTLS := opts.FlintlockdTLS
	if serverTLS == nil {
		serverTLS = &fakeflintlock.TLS{
			CertFile:     filepath.Join(h.CertDir, execagent.FlintlockdCertFile),
			KeyFile:      filepath.Join(h.CertDir, execagent.FlintlockdKeyFile),
			ClientCAFile: filepath.Join(h.CertDir, execagent.FlintlockdClientCAFile),
		}
	}
	h.Fake = fakeflintlock.New(fakeflintlock.Config{
		Name: h.Node, Listen: h.flintlockd, Version: "test",
		ExecEnabled: !opts.ExecDisabled, TLS: serverTLS,
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
	if opts.FlintlockdTLS == nil {
		// The agent's first checks found no flintlockd; a test starts once
		// it has reached the one just started.
		Eventually(t, "the exec agent reached flintlockd", func() bool {
			a := h.ReadNode().Annotations
			return a[execagent.AnnotationReason] != "" && a[execagent.AnnotationReason] != execagent.ReasonFlintlockdNotReady
		})
	}
	return h
}

// FakeHostPrerequisites makes stand-ins for the Host prerequisites the
// agent checks on the Host itself, since there is no KVM here: /dev/null as
// the KVM device node, since a test cannot make a character device; a sysfs
// directory listing the KVM device; and a sysfs block directory whose one
// device-mapper device, dm-0, is containerd's thin pool under its default
// name.
func FakeHostPrerequisites(t testing.TB) (kvmDevice, kvmSysfsDir, sysBlockDir string) {
	t.Helper()
	dir := t.TempDir()
	kvmSysfsDir = filepath.Join(dir, "sys", "class", "misc", "kvm")
	if err := os.MkdirAll(kvmSysfsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kvmSysfsDir, "dev"), []byte("10:232\n"), 0o444); err != nil {
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
	return "/dev/null", kvmSysfsDir, sysBlockDir
}

// Log is what the Exec Agent has logged.
func (h *Host) Log() string { return h.logs.String() }

// Certificates are the certificates of the Exec Agent running now, nil
// when it is stopped.
func (h *Host) Certificates() *execagent.Certificates {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.certs
}

// Config is the agent's configuration on this Host.
func (h *Host) Config() *execagent.Config {
	open := h.opts.ExecOpenTimeout
	if open == 0 {
		open = 2 * time.Second
	}
	cfg := &execagent.Config{
		HostNode:          h.Node,
		Flintlockd:        h.flintlockd,
		TrustDomain:       TrustDomain,
		FlintlockdCertDir: h.CertDir,
		CABundle:          execagent.CABundleRef{Namespace: AgentNamespace, Name: controller.DefaultCABundleConfigMap},
		ExecOpenTimeout:   open,
		CallTimeout:       5 * time.Second,
		NotReadyDir:       h.NotReadyDir,
		KVMDevice:         h.KVMDevice,
		KVMSysfsDir:       h.KVMSysfsDir,
		SysBlockDir:       h.SysBlockDir,
		DrainTimeout:      h.opts.DrainTimeout,
		Guard:             execagent.Guard{Namespace: AgentNamespace},
		SyncInterval:      50 * time.Millisecond,
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
	certs := execagent.NewCertificates(kube, cfg, h.opts.CertificateClock)
	fl, err := execagent.DialFlintlockd(cfg.Flintlockd, certs, []net.IP{net.ParseIP(HostAddress)})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	claims, err := execagent.NewKubeClaims(ctx, restConfig, time.Minute)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	go func() { _ = claims.Run(ctx) }()

	logger := funcr.New(func(prefix, args string) {
		_, _ = fmt.Fprintln(h.logs, prefix, args)
	}, funcr.Options{Verbosity: 1})
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer func() { _ = fl.Close() }()
		done <- execagent.Run(ctx, execagent.Options{
			Config: cfg, Kube: kube, Claims: claims, Flintlockd: fl, Certificates: certs, Listener: listener, Ready: ready,
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
	h.cancel, h.done, h.certs = cancel, done, certs
	h.mu.Unlock()
}

// StopAgent stops the Exec Agent, as a restart of its container does. Its
// in-flight streams are cut after the agent's stop grace.
func (h *Host) StopAgent() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done, h.certs = nil, nil, nil
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
