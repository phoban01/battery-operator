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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/execagent/execagenttest"
)

// The tests of this package's _test files run the Exec Agent against a real
// kube-apiserver serving this project's CRDs, with the agent's
// shipped RBAC and admission policy, and a fake flintlockd served over
// mutual TLS (execagenttest). Callers are real ServiceAccounts with real
// tokens, which the agent reviews with real TokenReviews. Without the
// envtest binaries, which `make test` provides, they skip.

// env is the shared API server, nil when there is none.
var env *execagenttest.Env

func TestMain(m *testing.M) {
	e, err := execagenttest.Start()
	switch {
	case errors.Is(err, execagenttest.ErrNoAssets):
	case err != nil:
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	default:
		env = e
	}
	code := m.Run()
	if env != nil {
		_ = env.Stop()
	}
	os.Exit(code)
}

// testTimeout bounds every wait in these tests; hitting it means a hang.
const testTimeout = 30 * time.Second

// needEnv skips a test without an API server.
func needEnv(t *testing.T) {
	t.Helper()
	if env == nil {
		t.Skip(execagenttest.ErrNoAssets)
	}
}

// fixture is one Host with its Exec Agent, a namespace standing for a
// consumer's, and the consumer's identity in it.
type fixture struct {
	t      *testing.T
	host   *execagenttest.Host
	ns     string
	holder execagenttest.Identity
}

func newFixture(t *testing.T, opts execagenttest.HostOptions) *fixture {
	t.Helper()
	needEnv(t)
	ns := env.Namespace(t)
	return &fixture{t: t, host: env.NewHost(t, opts), ns: ns, holder: env.ServiceAccountToken(t, ns, "holder")}
}

// bind writes a Bound claim of the holder on the Host's MicroVM, as the
// Claim Controller does when battery grants one, expiring in an hour, with
// its Secret, and gives the holder a claim token bound to that Secret.
func (f *fixture) bind(name string) {
	f.t.Helper()
	env.PutClaim(f.t, f.ns, name, f.holder.Name, execagenttest.ClaimStatus{
		Phase: batteryv1alpha1.MicroVMClaimBound, VMUID: f.host.VMUID, HostNode: f.host.Node, ExpiresAt: time.Now().Add(time.Hour),
	})
	f.holder.Token = env.ClaimToken(f.t, f.ns, f.holder.Name, name+execagent.ExecSecretSuffix)
}

// bearer is a static bearer token.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}
func (bearer) RequireTransportSecurity() bool { return true }

// conn connects to the agent at address, verifying its certificate against
// caFile, with the given token or none.
func conn(t *testing.T, address, caFile, token string) *grpc.ClientConn {
	t.Helper()
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	opts := []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer(token)))
	}
	c, err := grpc.NewClient(address, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// rawExec opens a MicroVMExec client on the agent directly, verifying its
// certificate, with the given token or none, so that a test sees the exact
// status the agent answers with.
func (f *fixture) rawExec(token string) execv1.MicroVMExecClient {
	f.t.Helper()
	return execv1.NewMicroVMExecClient(conn(f.t, f.host.Address, env.ServingCAFile, token))
}

// result is how one exchange ended.
type result struct {
	exitCode int32
	gotExit  bool
	stdout   string
	stderr   string
	err      error
}

// exchange runs start through a raw exchange, sending stdin when it is not
// empty, and returns how the exchange ended. onStdout, when set, sees each
// stdout chunk as it arrives.
func exchange(ctx context.Context, client execv1.MicroVMExecClient, start *execv1.ExecStart, stdin string, onStdout func(string)) result {
	stream, err := client.ExecCommand(ctx)
	if err != nil {
		return result{err: err}
	}
	start.HasStdin = stdin != ""
	if err := stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}}); err != nil && !errors.Is(err, io.EOF) {
		return result{err: err}
	}
	for chunk := range chunks(stdin, 32*1024) {
		if err := stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte(chunk)}}); err != nil {
			break
		}
	}
	_ = stream.CloseSend()
	var r result
	var out, errOut strings.Builder
	for {
		resp, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				r.err = err
			}
			r.stdout, r.stderr = out.String(), errOut.String()
			return r
		}
		switch p := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Stdout:
			out.Write(p.Stdout)
			if onStdout != nil {
				onStdout(string(p.Stdout))
			}
		case *execv1.ExecCommandResponse_Stderr:
			errOut.Write(p.Stderr)
		case *execv1.ExecCommandResponse_ExitCode:
			r.exitCode, r.gotExit = p.ExitCode, true
		}
	}
}

// chunks splits s into pieces of at most n bytes.
func chunks(s string, n int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for len(s) > 0 {
			k := min(n, len(s))
			if !yield(s[:k]) {
				return
			}
			s = s[k:]
		}
	}
}

// start is an ExecStart for cmd on the fixture's MicroVM.
func (f *fixture) start(cmd string) *execv1.ExecStart {
	return &execv1.ExecStart{Uid: f.host.VMUID, Cmd: cmd}
}

// ran reports whether the Host's fake flintlockd accepted cmd, which is how
// a test tells that a command reached the Host at all.
func (f *fixture) ran(cmd string) bool {
	for _, e := range f.host.Fake.Execs() {
		if e.GetCmd() == cmd {
			return true
		}
	}
	return false
}

// cutProxy forwards TCP connections to an address until cut is called,
// which closes every connection it carries at once, as a network fault or
// a node losing its route would.
type cutProxy struct {
	l     net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, to string) *cutProxy {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(execagenttest.HostAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{l: l}
	t.Cleanup(func() { _ = l.Close(); p.cut() })
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", to)
			if err != nil {
				_ = in.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	return p
}

func (p *cutProxy) addr() string { return p.l.Addr().String() }

func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}
