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

package execagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/hostcheck"
)

// Names the tests share.
const (
	testHolder    = "holder"
	testClaimName = "claim"
	testCaller    = "system:serviceaccount:" + testNamespace + ":" + testHolder
	testNamespace = "unit"
	testVersion   = "test"
	testCommand   = "run"
	testHost      = "host"
)

// loopback is the one Host address these tests run on.
var loopback = []net.IP{net.IPv4(127, 0, 0, 1)}

// authenticated answers every TokenReview as the Holder's token, with the
// id of testClaimToken.
func authenticated() (*authenticationv1.TokenReview, error) {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true, Audiences: []string{DefaultTokenAudience},
		User: authenticationv1.UserInfo{
			Username: testCaller,
			Extra:    map[string]authenticationv1.ExtraValue{credentialIDExtra: {"JTI=" + testJTI}},
		},
	}}, nil
}

// stubClaims is a ClaimLookup of fixed answers.
type stubClaims struct {
	claims []Claim
	err    error
}

func (s *stubClaims) Claim(_ context.Context, namespace, name string) (*Claim, error) {
	if s.err != nil {
		return nil, s.err
	}
	for _, c := range s.claims {
		if c.Namespace == namespace && c.Name == name {
			return &c, nil
		}
	}
	return nil, nil
}
func (s *stubClaims) BoundOnHost(context.Context, string) ([]Claim, error) { return s.claims, s.err }

// boundClaim is a claim that authorizes testClaimToken on uid.
func boundClaim(uid string) Claim {
	return Claim{
		Namespace: testNamespace, Name: testClaimName, ServiceAccountName: testHolder,
		Phase: batteryv1alpha1.MicroVMClaimBound, VMUID: uid, HostNode: testHost, ExpiresAt: time.Now().Add(time.Hour),
	}
}

// serveFake serves a fake flintlockd over mutual TLS on loopback until the
// test ends, with one CREATED MicroVM, and returns it, its certificates and
// the MicroVM's uid.
func serveFake(t *testing.T, cfg fakeflintlock.Config) (*fakeflintlock.Server, *fakeflintlock.TestCerts, string) {
	t.Helper()
	certs, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLS = certs.ServerTLS(true)
	fake := fakeflintlock.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- fake.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served })
	select {
	case <-fake.Ready():
	case err := <-served:
		t.Fatalf("the fake flintlockd did not start: %v", err)
	}
	vm, err := fake.CreateMicroVM(&types.MicroVMSpec{Id: "vm", Namespace: testNamespace})
	if err != nil {
		t.Fatal(err)
	}
	return fake, certs, vm.GetSpec().GetUid()
}

// staticCertificates are Certificates that hold the client certificate and
// the server certificate of from, as the agent's client certificate for
// flintlockd and its serving certificate, and trust ca's CA as the serving
// CA. from may be nil, for Certificates that hold nothing.
func staticCertificates(t *testing.T, from, ca *fakeflintlock.TestCerts) *Certificates {
	t.Helper()
	c := NewCertificates(nil, &Config{}, nil)
	if from != nil {
		for k, files := range map[certKind][2]string{
			flintlockdClient: {from.ClientCertFile, from.ClientKeyFile},
			agentServing:     {from.ServerCertFile, from.ServerKeyFile},
		} {
			pair, err := tls.LoadX509KeyPair(files[0], files[1])
			if err != nil {
				t.Fatal(err)
			}
			c.set(k, &held{cert: &pair})
		}
	}
	data, err := os.ReadFile(ca.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	c.servingCAs = x509.NewCertPool()
	c.servingCAs.AppendCertsFromPEM(data)
	return c
}

// unitAgent serves the exec API over TLS in front of a fake flintlockd
// served over mutual TLS, with a fake API server client whose TokenReviews
// are answered by review, and returns the connection to reach it with, the
// fake flintlockd and the uid of a CREATED MicroVM on it.
func unitAgent(t *testing.T, review func() (*authenticationv1.TokenReview, error), claims ClaimLookup) (*grpc.ClientConn, *fakeflintlock.Server, string) {
	t.Helper()
	fake, certs, uid := serveFake(t, fakeflintlock.Config{Name: "h", ExecEnabled: true, Version: testVersion})
	held := staticCertificates(t, certs, certs)
	fl, err := DialFlintlockd(fake.Addr(), held, loopback)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.Close() })

	kube := kubefake.NewClientset()
	kube.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		r, err := review()
		return true, r, err
	})
	s := &server{
		hostNode: testHost, fl: fl, claims: claims, clk: clock.Real{}, log: logr.Discard(),
		authn: &authenticator{
			reviews: kube.AuthenticationV1().TokenReviews(), audiences: []string{DefaultTokenAudience}, timeout: time.Second,
		},
		openTimeout: time.Second, callTimeout: time.Second,
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(held.servingTLS())),
		grpc.ChainUnaryInterceptor(s.unaryInterceptor), grpc.ChainStreamInterceptor(s.streamInterceptor))
	s.register(srv)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	creds, err := credentials.NewClientTLSFromFile(certs.CAFile, "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, fake, uid
}

// withToken is a context carrying a bearer token.
func withToken(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testClaimToken)
}

// runExec execs testCommand as a caller with a token and returns the status the
// exchange ended with.
func runExec(t *testing.T, conn *grpc.ClientConn, uid string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(withToken(context.Background()), 10*time.Second)
	defer cancel()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		return err
	}
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{Uid: uid, Cmd: testCommand}}})
	_ = stream.CloseSend()
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			return nil
		}
	}
}

// ran reports whether the fake flintlockd accepted a command.
func ran(fake *fakeflintlock.Server, cmd string) bool {
	for _, e := range fake.Execs() {
		if e.GetCmd() == cmd {
			return true
		}
	}
	return false
}

//= docs/requirements/05-exec-agent.md#authorization
//= type=test
//# If the TokenReview or the claim lookup of a request cannot be
//# completed, then the Exec Agent SHALL refuse the request.

// TestRefusedWhenTheReviewOrTheLookupCannotBeCompleted runs the agent with
// an API server that fails the TokenReview, and with one that authenticates
// the caller but whose claims cannot be looked up. Both requests are
// refused as unavailable and neither reaches flintlockd; with both
// answering and a good claim, the same request runs.
func TestRefusedWhenTheReviewOrTheLookupCannotBeCompleted(t *testing.T) {
	t.Parallel()

	t.Run("the TokenReview fails", func(t *testing.T) {
		t.Parallel()
		lookup := &stubClaims{}
		conn, fake, uid := unitAgent(t, func() (*authenticationv1.TokenReview, error) {
			return nil, errors.New("the API server is down")
		}, lookup)
		lookup.claims = []Claim{boundClaim(uid)}
		if err := runExec(t, conn, uid); status.Code(err) != codes.Unavailable {
			t.Errorf("exec = %v, want unavailable", err)
		}
		if ran(fake, testCommand) {
			t.Error("the command ran although the token could not be reviewed")
		}
	})
	t.Run("the claim lookup fails", func(t *testing.T) {
		t.Parallel()
		conn, fake, uid := unitAgent(t, authenticated, &stubClaims{err: errors.New("the API server is down")})
		if err := runExec(t, conn, uid); status.Code(err) != codes.Unavailable {
			t.Errorf("exec = %v, want unavailable", err)
		}
		if ran(fake, testCommand) {
			t.Error("the command ran although the claims could not be looked up")
		}
	})
	t.Run("both answer", func(t *testing.T) {
		t.Parallel()
		lookup := &stubClaims{}
		conn, fake, uid := unitAgent(t, authenticated, lookup)
		lookup.claims = []Claim{boundClaim(uid)}
		if err := runExec(t, conn, uid); err != nil {
			t.Errorf("exec = %v", err)
		}
		if !ran(fake, testCommand) {
			t.Error("the command did not run")
		}
	})
}

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// TestASecondExecStartEndsTheExchange authorizes an exchange for a MicroVM
// and then sends a second ExecStart on it. The claim was checked for the
// first message only, so the second must never reach flintlockd: the
// exchange ends as an invalid argument with no exit status, and the
// command the second ExecStart named is never accepted.
func TestASecondExecStartEndsTheExchange(t *testing.T) {
	t.Parallel()
	lookup := &stubClaims{}
	conn, fake, uid := unitAgent(t, authenticated, lookup)
	lookup.claims = []Claim{boundClaim(uid)}
	started := make(chan struct{})
	fake.SetExec(func(ctx context.Context, _ *fakeflintlock.Exec) (int32, error) {
		close(started)
		<-ctx.Done()
		return 0, nil
	})

	ctx, cancel := context.WithTimeout(withToken(context.Background()), 10*time.Second)
	defer cancel()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := func(cmd string) *execv1.ExecCommandRequest {
		return &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{Uid: uid, Cmd: cmd}}}
	}
	if err := stream.Send(start("first")); err != nil {
		t.Fatal(err)
	}
	// The first command is running, so the exchange is past the claim check
	// and relaying.
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the authorized command never started")
	}
	if err := stream.Send(start("second")); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	for {
		resp, err := stream.Recv()
		if err == nil {
			if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
				t.Fatal("the exchange carried an exit status")
			}
			continue
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("exchange ended with %v, want invalid argument", err)
		}
		break
	}
	if ran(fake, "second") {
		t.Error("the command of the second ExecStart reached flintlockd")
	}
}

//= docs/requirements/05-exec-agent.md#serving
//= type=test
//# The Exec Agent's exec API SHALL be flintlock's
//# `microvmexec.services.api.v1alpha1` service, together with the
//# `ServerInfo` and `GetMicroVM` calls of its `microvm.services.api.v1alpha1`
//# service.

// TestExecAPIIsFlintlocksOwn calls the exec API with flintlock's own
// generated clients: ServerInfo and GetMicroVM are relayed from flintlockd,
// ExecCommand runs, and every other call of the MicroVM service, which
// would create, delete or list MicroVMs, is unimplemented and changes
// nothing.
func TestExecAPIIsFlintlocksOwn(t *testing.T) {
	t.Parallel()
	lookup := &stubClaims{}
	conn, fake, uid := unitAgent(t, authenticated, lookup)
	lookup.claims = []Claim{boundClaim(uid)}
	ctx, cancel := context.WithTimeout(withToken(context.Background()), 10*time.Second)
	defer cancel()
	vms := mvmv1.NewMicroVMClient(conn)

	info, err := vms.ServerInfo(ctx, &emptypb.Empty{})
	if err != nil || info.GetVersion().GetVersion() != testVersion || !info.GetExec().GetEnabled() {
		t.Errorf("ServerInfo = (%v, %v), want flintlockd's answer", info, err)
	}
	got, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil || got.GetMicrovm().GetSpec().GetUid() != uid {
		t.Errorf("GetMicroVM = (%v, %v), want the microvm", got, err)
	}
	if err := runExec(t, conn, uid); err != nil || !ran(fake, testCommand) {
		t.Errorf("ExecCommand = %v, want it relayed", err)
	}

	calls := map[string]func() error{
		"CreateMicroVM": func() error {
			_, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{Microvm: &types.MicroVMSpec{Id: "new", Namespace: testNamespace}})
			return err
		},
		"DeleteMicroVM": func() error {
			_, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid})
			return err
		},
		"ListMicroVMs": func() error {
			_, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: testNamespace})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.Unimplemented {
			t.Errorf("%s = %v, want unimplemented", name, err)
		}
	}
	if n := len(fake.MicroVMs()); n != 1 {
		t.Errorf("flintlockd holds %d microvms, want the one it started with", n)
	}
}

//= docs/requirements/05-exec-agent.md#serving
//= type=test
//# The Exec Agent SHALL serve its exec API over TLS on the Host's
//# internal address, with a serving certificate that names that address.

// TestServingCertificateNamesTheAddress checks that the agent puts a signed
// serving certificate in use only when it names the exec API's address, is
// for the key the agent generated, carries the agent's SPIFFE ID and chains
// to the published serving CA. TestAgentRequestsItsCertificates checks that
// the agent serves on its Node's internal address with the certificate it
// obtained.
func TestServingCertificateNamesTheAddress(t *testing.T) {
	t.Parallel()
	ca, other := newUnitCA(t), newUnitCA(t)
	c := NewCertificates(nil, validConfig(), nil)
	if err := c.setSpecs("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	c.servingCAs, c.clientCAs = ca.pool, ca.pool
	key, strangerKey := newUnitKey(t), newUnitKey(t)
	agentID := hostcert.ExecAgentID("example.test", "h")
	serving := x509.ExtKeyUsageServerAuth

	if _, err := c.check(agentServing, ca.issue(t, key, agentID, "127.0.0.1", serving), key); err != nil {
		t.Errorf("a certificate for 127.0.0.1 on 127.0.0.1: %v", err)
	}
	for what, chain := range map[string][]byte{
		"a certificate for another address":   ca.issue(t, key, agentID, "10.0.0.7", serving),
		"a certificate for another identity":  ca.issue(t, key, hostcert.HostID("example.test", "h"), "127.0.0.1", serving),
		"a certificate for another key":       ca.issue(t, strangerKey, agentID, "127.0.0.1", serving),
		"a certificate from another CA":       other.issue(t, key, agentID, "127.0.0.1", serving),
		"a certificate for client auth":       ca.issue(t, key, agentID, "127.0.0.1", x509.ExtKeyUsageClientAuth),
		"a request the Operator did not sign": nil,
	} {
		if _, err := c.check(agentServing, chain, key); err == nil {
			t.Errorf("%s was put in use, want it refused", what)
		}
	}
}

// unitCA is a throwaway CA.
type unitCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newUnitCA(t *testing.T) *unitCA {
	t.Helper()
	key := newUnitKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unit CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &unitCA{cert: cert, key: key, pool: pool}
}

func newUnitKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// issue signs a certificate for key's public key naming uri and ip, and
// returns it PEM encoded.
func (ca *unitCA) issue(t *testing.T, key *ecdsa.PrivateKey, uri, ip string, usage x509.ExtKeyUsage) []byte {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "h"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		URIs: []*url.URL{u}, IPAddresses: []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: der})
}

// validConfig is a configuration that passes Validate on loopback.
func validConfig() *Config {
	cfg := &Config{
		HostNode: "h", Flintlockd: "127.0.0.1:9090", TrustDomain: "example.test",
		CABundle: CABundleRef{Namespace: "ns"},
		Guard:    Guard{Namespace: "ns"},
	}
	cfg.ApplyDefaults()
	return cfg
}

//= docs/requirements/05-exec-agent.md#serving
//= type=test
//# The Exec Agent SHALL reach only its own Host's `flintlockd`,
//# over TLS with its client certificate for `flintlockd`, and SHALL refuse to
//# start with a `flintlockd` endpoint that is not an address of its own Host.

// TestFlintlockdOnlyOnThisHostOverMutualTLS checks that the configuration
// and the dialler both refuse a flintlockd endpoint that is not an address
// of this Host, and that the connection is mutual TLS: flintlockd answers
// the agent's client certificate, and refuses a client whose certificate
// its client CA did not sign; the agent refuses a flintlockd whose serving
// certificate the serving CA did not sign; and there is no mode without a
// client certificate.
func TestFlintlockdOnlyOnThisHostOverMutualTLS(t *testing.T) {
	t.Parallel()
	fake, certs, _ := serveFake(t, fakeflintlock.Config{Name: "h", ExecEnabled: true, Version: testVersion})

	for _, endpoint := range []string{"192.0.2.10:9090", "localhost:9090", "dns:///flintlockd:9090", "0.0.0.0:9090", "unix:///run/flintlockd.sock"} {
		if _, err := DialFlintlockd(endpoint, staticCertificates(t, certs, certs), loopback); err == nil {
			t.Errorf("DialFlintlockd(%q) succeeded, want it refused", endpoint)
		}
		cfg := validConfig()
		cfg.Flintlockd = endpoint
		if err := cfg.Validate(loopback); err == nil || !strings.Contains(err.Error(), "flintlockd:") {
			t.Errorf("a configuration with flintlockd at %q = %v, want it refused", endpoint, err)
		}
	}
	if err := validConfig().Validate(loopback); err != nil {
		t.Fatalf("a configuration with flintlockd on this Host: %v", err)
	}
	if _, err := DialFlintlockd(fake.Addr(), nil, loopback); err == nil {
		t.Error("DialFlintlockd without certificates succeeded, want it refused")
	}

	serverInfo := func(c *Certificates) error {
		fl, err := DialFlintlockd(fake.Addr(), c, loopback)
		if err != nil {
			return err
		}
		defer func() { _ = fl.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = fl.vms.ServerInfo(ctx, &emptypb.Empty{})
		return err
	}
	if err := serverInfo(staticCertificates(t, certs, certs)); err != nil {
		t.Errorf("ServerInfo with the agent's client certificate: %v", err)
	}
	if err := serverInfo(staticCertificates(t, nil, certs)); err == nil {
		t.Error("flintlockd answered an agent that holds no client certificate yet")
	}
	other, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := serverInfo(staticCertificates(t, other, certs)); err == nil {
		t.Error("flintlockd answered a client certificate its client CA did not sign")
	}
	if err := serverInfo(staticCertificates(t, certs, other)); err == nil {
		t.Error("the agent talked to a flintlockd whose serving certificate the serving CA did not sign")
	}
}

//= docs/requirements/05-exec-agent.md#host-checks
//= type=test
//# The Exec Agent SHALL read not ready reasons from the directory
//# its configuration names.

// TestNotReadyReasonsComeFromTheConfiguredDirectory checks readiness twice
// against the same flintlockd, once with the configured directory holding a
// reason and once with it pointing at an empty directory beside it: only
// the configured directory counts, and the default is not read at all.
func TestNotReadyReasonsComeFromTheConfiguredDirectory(t *testing.T) {
	t.Parallel()
	fake, certs, _ := serveFake(t, fakeflintlock.Config{Name: "h", ExecEnabled: true, Version: testVersion})
	fl, err := DialFlintlockd(fake.Addr(), staticCertificates(t, certs, certs), loopback)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fl.Close() }()
	withReason, empty := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(withReason, "unit.service"), []byte("configured reason\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := validConfig()
	if cfg.NotReadyDir != hostcheck.DefaultNotReadyDir {
		t.Fatalf("the default directory is %q, want %q", cfg.NotReadyDir, hostcheck.DefaultNotReadyDir)
	}
	cfg.NotReadyDir = withReason
	fakePrerequisites(t, cfg)
	ctx := context.Background()
	if r := checkReadiness(ctx, cfg, fl); r.ready || r.reason != ReasonHostImageNotReady || !strings.Contains(r.message, "unit.service: configured reason") {
		t.Errorf("readiness with a reason in %s = %+v, want not ready with that reason", withReason, r)
	}
	cfg.NotReadyDir = empty
	if r := checkReadiness(ctx, cfg, fl); !r.ready {
		t.Errorf("readiness with the empty directory %s configured = %+v, want ready", empty, r)
	}
}
