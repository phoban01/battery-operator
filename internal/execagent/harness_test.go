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

package execagent_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/liquidmetal-dev/flintlock/api/types"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// The tests of package execagent_test run the whole Exec Agent, execagent.Run,
// against fakes and no API server (ADR 0006): client-go's fake clientset
// stands in for the API server, with the TokenReviews,
// SelfSubjectReviews and CertificateSigningRequests the agent makes
// answered as the API server and the Operator's signer answer them;
// controller-runtime's fake client holds the claims, which the agent reads
// through its real claim lookup; and a fake flintlockd
// (internal/fakeflintlock) is served over mutual TLS on loopback with the
// certificates the agent obtains. What only a real API server shows, the
// review of real tokens, the admission policy and the agent's RBAC as
// shipped, is tested by the e2e suite (test/e2e).

const (
	// agentNamespace is the namespace of config/exec-agent, where the guard
	// pods live.
	agentNamespace = "battery-operator-system"
	// agentServiceAccount is the Exec Agent's ServiceAccount.
	agentServiceAccount = "battery-operator-exec-agent"
	// agentUser is the user name of agentServiceAccount.
	agentUser = "system:serviceaccount:" + agentNamespace + ":" + agentServiceAccount
	// hostAddress is the internal address of every Host's Node here, which
	// is where each Exec Agent listens, where each fake flintlockd listens,
	// and what the certificates name.
	hostAddress = "127.0.0.1"
	// trustDomain is the SPIFFE trust domain of the fake signer and of
	// every Exec Agent.
	trustDomain = "unit.example"
	// certificateMaxDuration is the longest validity the fake signer
	// issues, as the Operator's does by default.
	certificateMaxDuration = time.Hour
	// apiServerAudience is the audience of a token requested with none,
	// which the Exec Agent does not accept.
	apiServerAudience = "https://kubernetes.default.svc"
	// testTimeout bounds every wait in these tests; hitting it means a
	// hang.
	testTimeout = 30 * time.Second
)

// moduleRoot is the directory of go.mod: this file is two directories below
// it.
func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// eventually polls ok until it holds or testTimeout passes.
func eventually(t testing.TB, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", testTimeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeCluster is the API server of one Host's tests.
type fakeCluster struct {
	// Kube is the fake clientset, which the agent and the test share. The
	// test plays battery, the Host's kubelet and the Operator with it.
	Kube *kubefake.Clientset
	// Claims holds the MicroVMClaims.
	Claims client.Client
	// ServingCAFile is the serving CA's certificate, which signs every
	// Exec Agent's serving certificate and every flintlockd's: what a
	// consumer verifies an Exec Agent against.
	ServingCAFile string
	// ClientCAPEM is the client CA's certificate, as the CA bundle
	// publishes it.
	ClientCAPEM string

	servingCA, clientCA *testCA
	// identityNode is the node the agent's identity names, which a
	// SelfSubjectReview reports and a CertificateSigningRequest records;
	// empty names none.
	identityNode string
	seq          atomic.Int64

	mu     sync.Mutex
	tokens map[string]issuedToken
}

// issuedToken is what the fake API server knows of a token it issued.
type issuedToken struct {
	user      string
	jti       string
	audiences []string
	// secret is the Secret the token is bound to, nil for none.
	secret *corev1.Secret
}

var (
	secretsResource = corev1.SchemeGroupVersion.WithResource("secrets")
)

//= docs/requirements/08-test-doubles.md#test-environments
//# The unit tests SHALL test each subreconciler, and the rest of
//# the logic of the controllers and the Exec Agent, against the fake battery,
//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
//# API server.

// newFakeCluster is the API server of a Host whose Node is hostNode, at
// hostAddress: client-go's fake clientset holding that Node and the CA
// bundle the Operator publishes, with throwaway CAs, and answering
//   - a TokenReview as the API server does for the tokens token issued:
//     authenticated only for an audience the token was issued for, and
//     while the Secret it is bound to exists with the uid it had;
//   - a SelfSubjectReview as the agent's own, naming identityNode;
//   - a CertificateSigningRequest as the Operator's signer does, approved
//     and signed at once by the CA of its signer name, with the requester
//     recorded as the API server records it.
//
// The claims are in controller-runtime's fake client, indexed as the
// agent's claim cache is.
func newFakeCluster(t testing.TB, hostNode, identityNode string) *fakeCluster {
	t.Helper()
	c := &fakeCluster{identityNode: identityNode, tokens: map[string]issuedToken{}}
	var err error
	if c.servingCA, err = newTestCA("unit serving CA"); err != nil {
		t.Fatal(err)
	}
	if c.clientCA, err = newTestCA("unit flintlockd client CA"); err != nil {
		t.Fatal(err)
	}
	c.ServingCAFile = filepath.Join(t.TempDir(), "serving-ca.crt")
	if err := os.WriteFile(c.ServingCAFile, c.servingCA.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	c.ClientCAPEM = string(c.clientCA.certPEM)

	c.Kube = kubefake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: hostNode, UID: uuid.NewUUID()},
			Status:     corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: hostAddress}}},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: agentNamespace, Name: hostcert.CABundleConfigMap},
			Data:       map[string]string{hostcert.ServingCAKey: string(c.servingCA.certPEM), hostcert.ClientCAKey: c.ClientCAPEM},
		},
	)
	c.Kube.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		review := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		return true, c.review(review), nil
	})
	c.Kube.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		info := authenticationv1.UserInfo{Username: agentUser}
		if c.identityNode != "" {
			info.Extra = map[string]authenticationv1.ExtraValue{hostcert.NodeNameExtra: {c.identityNode}}
		}
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: info}}, nil
	})
	// The request is completed in place and stored by the fake's own
	// reactor, which comes after this one.
	c.Kube.PrependReactor("create", "certificatesigningrequests", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		csr := a.(k8stesting.CreateAction).GetObject().(*certificatesv1.CertificateSigningRequest)
		if csr.Name == "" {
			csr.Name = fmt.Sprintf("%s%d", csr.GenerateName, c.seq.Add(1))
		}
		csr.Spec.Username = agentUser
		if c.identityNode != "" {
			csr.Spec.Extra = map[string]certificatesv1.ExtraValue{hostcert.NodeNameExtra: {c.identityNode}}
		}
		chain, err := c.sign(csr)
		if err != nil {
			csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
				Type: certificatesv1.CertificateFailed, Status: corev1.ConditionTrue, Reason: "SignerFailed", Message: err.Error(),
			}}
			return false, nil, nil
		}
		csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
			Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue, Reason: "Approved",
		}}
		csr.Status.Certificate = chain
		return false, nil, nil
	})

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(batteryv1alpha1.AddToScheme(scheme))
	c.Claims = fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, execagent.HostNodeIndex, execagent.ClaimHostNode).
		Build()
	return c
}

// review answers a TokenReview as the API server does.
func (c *fakeCluster) review(r *authenticationv1.TokenReview) *authenticationv1.TokenReview {
	out := r.DeepCopy()
	c.mu.Lock()
	tok, ok := c.tokens[r.Spec.Token]
	c.mu.Unlock()
	if !ok {
		out.Status = authenticationv1.TokenReviewStatus{Error: "invalid bearer token"}
		return out
	}
	want := r.Spec.Audiences
	if len(want) == 0 {
		want = []string{apiServerAudience}
	}
	var audiences []string
	for _, a := range want {
		if slices.Contains(tok.audiences, a) {
			audiences = append(audiences, a)
		}
	}
	if len(audiences) == 0 {
		out.Status = authenticationv1.TokenReviewStatus{Error: fmt.Sprintf("token audiences %q is invalid for the target audiences %q", tok.audiences, want)}
		return out
	}
	if s := tok.secret; s != nil {
		obj, err := c.Kube.Tracker().Get(secretsResource, s.Namespace, s.Name)
		if err != nil {
			out.Status = authenticationv1.TokenReviewStatus{Error: fmt.Sprintf("secret %s/%s bound to the token: %v", s.Namespace, s.Name, err)}
			return out
		}
		if now, ok := obj.(*corev1.Secret); !ok || now.UID != s.UID {
			out.Status = authenticationv1.TokenReviewStatus{Error: fmt.Sprintf("the uid of secret %s/%s bound to the token has changed", s.Namespace, s.Name)}
			return out
		}
	}
	out.Status = authenticationv1.TokenReviewStatus{
		Authenticated: true, Audiences: audiences,
		User: authenticationv1.UserInfo{
			Username: tok.user,
			Extra:    map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/credential-id": {"JTI=" + tok.jti}},
		},
	}
	return out
}

// Token issues a token of the ServiceAccount namespace/name, as
// TokenRequest does: a JWT whose claims name the account, and the Secret
// bound when bound is set, for audiences, or for the API server's own when
// there are none.
func (c *fakeCluster) Token(namespace, name string, audiences []string, bound *corev1.Secret) string {
	if len(audiences) == 0 {
		audiences = []string{apiServerAudience}
	}
	user := "system:serviceaccount:" + namespace + ":" + name
	jti := string(uuid.NewUUID())
	k8s := map[string]any{"namespace": namespace, "serviceaccount": map[string]any{"name": name}}
	if bound != nil {
		k8s["secret"] = map[string]any{"name": bound.Name, "uid": string(bound.UID)}
	}
	payload, err := json.Marshal(map[string]any{"sub": user, "jti": jti, "aud": audiences, "kubernetes.io": k8s})
	if err != nil {
		panic(err)
	}
	enc := base64.RawURLEncoding
	token := enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + "." + enc.EncodeToString([]byte(jti))
	var secret *corev1.Secret
	if bound != nil {
		secret = bound.DeepCopy()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[token] = issuedToken{user: user, jti: jti, audiences: audiences, secret: secret}
	return token
}

// ClaimToken issues a claim token as the Client Library requests one
// (CC-002): a token of namespace/serviceAccount for the Exec Agent's
// audience, bound to the Secret secretName as it is now.
func (c *fakeCluster) ClaimToken(t testing.TB, namespace, serviceAccount, secretName string) string {
	t.Helper()
	return c.Token(namespace, serviceAccount, []string{execagent.DefaultTokenAudience}, c.Secret(t, namespace, secretName))
}

// Secret reads a Secret.
func (c *fakeCluster) Secret(t testing.TB, namespace, name string) *corev1.Secret {
	t.Helper()
	s, err := c.Kube.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret %s/%s: %v", namespace, name, err)
	}
	return s
}

// CreateSecret creates a Secret with a uid of its own, as the API server
// gives it.
func (c *fakeCluster) CreateSecret(t testing.TB, namespace, name string, owners ...metav1.OwnerReference) *corev1.Secret {
	t.Helper()
	s, err := c.Kube.CoreV1().Secrets(namespace).Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name, UID: uuid.NewUUID(), OwnerReferences: owners,
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating secret %s/%s: %v", namespace, name, err)
	}
	return s
}

// claimStatus is what the Claim Controller would write into a claim's
// status from battery's answer.
type claimStatus struct {
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
func (c *fakeCluster) PutClaim(t testing.TB, namespace, name, serviceAccount string, st claimStatus) {
	t.Helper()
	ctx := context.Background()
	claim := &batteryv1alpha1.MicroVMClaim{}
	err := c.Claims.Get(ctx, k8stypes.NamespacedName{Namespace: namespace, Name: name}, claim)
	exists := err == nil
	switch {
	case apierrors.IsNotFound(err):
		claim = &batteryv1alpha1.MicroVMClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: uuid.NewUUID()},
			Spec: batteryv1alpha1.MicroVMClaimSpec{
				PoolRef:            batteryv1alpha1.PoolReference{Name: "pool"},
				ServiceAccountName: serviceAccount,
			},
		}
		err = nil
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
	if exists {
		err = c.Claims.Update(ctx, claim)
	} else {
		err = c.Claims.Create(ctx, claim)
	}
	if err != nil {
		t.Fatalf("writing claim %s/%s: %v", namespace, name, err)
	}
	secret := name + execagent.ExecSecretSuffix
	if _, err := c.Kube.CoreV1().Secrets(namespace).Get(ctx, secret, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		c.CreateSecret(t, namespace, secret, metav1.OwnerReference{
			APIVersion: batteryv1alpha1.GroupVersion.String(), Kind: "MicroVMClaim", Name: claim.Name, UID: claim.UID,
		})
	}
}

// DeleteClaim deletes a claim, as its consumer does when it is done. There
// is no garbage collector here, so its Secret stays, as it does for the
// few seconds a real one takes.
func (c *fakeCluster) DeleteClaim(t testing.TB, namespace, name string) {
	t.Helper()
	err := c.Claims.Delete(context.Background(), &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting claim %s/%s: %v", namespace, name, err)
	}
}

// Requests are the CertificateSigningRequests the Exec Agent has made.
func (c *fakeCluster) Requests(t testing.TB) []certificatesv1.CertificateSigningRequest {
	t.Helper()
	list, err := c.Kube.CertificatesV1().CertificateSigningRequests().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// sign issues the certificate a CertificateSigningRequest asks for, from
// the CA of its signer name, for what the request names, with the usages
// it asks for and the validity it asks for up to certificateMaxDuration,
// backdated five minutes.
// Deciding whether to sign is the Operator's, and its own tests cover it
// (internal/controller).
func (c *fakeCluster) sign(csr *certificatesv1.CertificateSigningRequest) ([]byte, error) {
	block, _ := pem.Decode(csr.Spec.Request)
	if block == nil {
		return nil, errors.New("spec.request holds no PEM block")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, err
	}
	ca := c.servingCA
	switch csr.Spec.SignerName {
	case hostcert.ClientSigner:
		ca = c.clientCA
	case hostcert.ServingSigner, hostcert.ExecAgentServingSigner:
	default:
		return nil, fmt.Errorf("no CA for the signer %s", csr.Spec.SignerName)
	}
	validity := certificateMaxDuration
	if s := csr.Spec.ExpirationSeconds; s != nil && time.Duration(*s)*time.Second < validity {
		validity = time.Duration(*s) * time.Second
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now().Truncate(time.Second)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: req.Subject.CommonName},
		// Backdated as the Operator backdates, so that a clock a little
		// behind, such as a test's fake one, finds it valid.
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(validity),
		URIs:        req.URIs,
		IPAddresses: req.IPAddresses,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	for _, u := range csr.Spec.Usages {
		switch u {
		case certificatesv1.UsageServerAuth:
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		case certificatesv1.UsageClientAuth:
			tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, req.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// testCA is a throwaway CA.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(cn string) (*testCA, error) {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &testCA{cert: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// hostOptions shape one Host.
type hostOptions struct {
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

// testHost is one Host: its fake API server, its Node, its fake
// flintlockd, one CREATED MicroVM on it, and its Exec Agent.
type testHost struct {
	*fakeCluster
	t    testing.TB
	opts hostOptions
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
	// Address is where the Exec Agent serves, hostAddress and its port.
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

// hostSeq names the Hosts.
var hostSeq atomic.Int64

// newHost creates a Host with an API server of its own, and starts its
// Exec Agent with an identity that names its Node. Once the agent has
// obtained its certificates, it starts a fake flintlockd on loopback with
// the files the agent wrote, over mutual TLS, with one CREATED MicroVM, as
// the Host Image starts flintlockd once they exist. Everything stops when
// the test ends.
func newHost(t testing.TB, opts hostOptions) *testHost {
	t.Helper()
	ctx := context.Background()
	node := fmt.Sprintf("host-%d", hostSeq.Add(1))
	h := &testHost{
		fakeCluster: newFakeCluster(t, node, node),
		t:           t, opts: opts, Node: node, logs: &syncBuffer{},
		CertDir: filepath.Join(t.TempDir(), "flintlockd"),
	}

	// flintlockd's port is chosen now, since the agent is configured with
	// it, and flintlockd starts only after the agent.
	probe, err := net.Listen("tcp", net.JoinHostPort(hostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	h.flintlockd = probe.Addr().String()
	_ = probe.Close()
	h.NotReadyDir = filepath.Join(t.TempDir(), "not-ready.d")
	h.KVMDevice, h.KVMSysfsDir, h.SysBlockDir = fakeHostPrerequisites(t)

	listener, err := net.Listen("tcp", net.JoinHostPort(hostAddress, "0"))
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
		select {
		case <-served:
		case <-time.After(testTimeout):
			t.Errorf("the fake flintlockd of %s did not stop in %s", h.Node, testTimeout)
		}
	})
	select {
	case <-h.Fake.Ready():
	case err := <-served:
		t.Fatalf("the fake flintlockd did not start: %v", err)
	case <-time.After(testTimeout):
		t.Fatalf("the fake flintlockd did not start in %s", testTimeout)
	}
	vm, err := h.Fake.CreateMicroVM(&types.MicroVMSpec{Id: "vm", Namespace: "unit"})
	if err != nil {
		t.Fatalf("creating a microvm: %v", err)
	}
	h.VMUID = vm.GetSpec().GetUid()
	if opts.FlintlockdTLS == nil {
		// The agent's first checks found no flintlockd; a test starts once
		// it has reached the one just started.
		eventually(t, "the exec agent reached flintlockd", func() bool {
			a := h.ReadNode().Annotations
			return a[execagent.AnnotationReason] != "" && a[execagent.AnnotationReason] != execagent.ReasonFlintlockdNotReady
		})
	}
	return h
}

// fakeHostPrerequisites makes stand-ins for the Host prerequisites the
// agent checks on the Host itself, since there is no KVM here: /dev/null as
// the KVM device node, since a test cannot make a character device; a sysfs
// directory listing the KVM device; and a sysfs block directory whose one
// device-mapper device, dm-0, is containerd's thin pool under its default
// name.
func fakeHostPrerequisites(t testing.TB) (kvmDevice, kvmSysfsDir, sysBlockDir string) {
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
func (h *testHost) Log() string { return h.logs.String() }

// Certificates are the certificates of the Exec Agent running now, nil
// when it is stopped.
func (h *testHost) Certificates() *execagent.Certificates {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.certs
}

// Config is the agent's configuration on this Host.
func (h *testHost) Config() *execagent.Config {
	open := h.opts.ExecOpenTimeout
	if open == 0 {
		open = 2 * time.Second
	}
	cfg := &execagent.Config{
		HostNode:          h.Node,
		Flintlockd:        h.flintlockd,
		TrustDomain:       trustDomain,
		FlintlockdCertDir: h.CertDir,
		CABundle:          execagent.CABundleRef{Namespace: agentNamespace, Name: hostcert.CABundleConfigMap},
		ExecOpenTimeout:   open,
		CallTimeout:       5 * time.Second,
		NotReadyDir:       h.NotReadyDir,
		KVMDevice:         h.KVMDevice,
		KVMSysfsDir:       h.KVMSysfsDir,
		SysBlockDir:       h.SysBlockDir,
		DrainTimeout:      h.opts.DrainTimeout,
		Guard:             execagent.Guard{Namespace: agentNamespace},
		SyncInterval:      50 * time.Millisecond,
	}
	cfg.ApplyDefaults()
	return cfg
}

// start runs the Exec Agent on listener, with the Host's fake API server
// and its real claim lookup over the fake claims.
func (h *testHost) start(listener net.Listener) {
	h.t.Helper()
	cfg := h.Config()
	certs := execagent.NewCertificates(h.Kube, cfg, h.opts.CertificateClock)
	fl, err := execagent.DialFlintlockd(cfg.Flintlockd, certs, []net.IP{net.ParseIP(hostAddress)})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logger := funcr.New(func(prefix, args string) {
		_, _ = fmt.Fprintln(h.logs, prefix, args)
	}, funcr.Options{Verbosity: 1})
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer func() { _ = fl.Close() }()
		done <- execagent.Run(ctx, execagent.Options{
			Config: cfg, Kube: h.Kube, Claims: execagent.NewKubeClaimsOver(h.Claims, h.Claims), Flintlockd: fl,
			Certificates: certs, Listener: listener, Ready: ready,
			HostAddresses: []net.IP{net.ParseIP(hostAddress)}, Logger: logger,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		h.t.Fatalf("the exec agent stopped before it was ready: %v\n%s", err, h.logs.String())
	case <-time.After(testTimeout):
		cancel()
		h.t.Fatalf("the exec agent was not ready in %s\n%s", testTimeout, h.logs.String())
	}
	h.mu.Lock()
	h.cancel, h.done, h.certs = cancel, done, certs
	h.mu.Unlock()
}

// StopAgent stops the Exec Agent, as a restart of its container does. Its
// in-flight streams are cut after the agent's stop grace.
func (h *testHost) StopAgent() {
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
	case <-time.After(testTimeout):
		h.t.Errorf("the exec agent of %s did not stop in %s", h.Node, testTimeout)
	}
}

// StartAgent starts the Exec Agent again on the same address, as the
// restarted container does.
func (h *testHost) StartAgent() {
	h.t.Helper()
	var listener net.Listener
	var err error
	deadline := time.Now().Add(testTimeout)
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
func (h *testHost) ReadNode() *corev1.Node {
	h.t.Helper()
	node, err := h.Kube.CoreV1().Nodes().Get(context.Background(), h.Node, metav1.GetOptions{})
	if err != nil {
		h.t.Fatalf("reading the host's node: %v", err)
	}
	return node
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
