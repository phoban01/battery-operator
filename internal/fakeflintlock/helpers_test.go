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

package fakeflintlock

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// testTimeout bounds every wait in these tests; a hit means a hang, not a
// slow machine.
const testTimeout = 30 * time.Second

var testEpoch = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// missingUID names no MicroVM.
const missingUID = "missing"

// newTestServer builds a Server and closes it when the test ends.
func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s := New(cfg)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// serve runs Serve in the background until the test ends or the returned
// stop function is called, which returns Serve's result.
func serve(t *testing.T, s *Server) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx) }()
	select {
	case <-s.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("Serve exited before listening: %v", err)
	case <-time.After(testTimeout):
		cancel()
		t.Fatal("Serve did not start listening")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-errc:
			case <-time.After(testTimeout):
				result = errors.New("serve did not return after cancel")
			}
		})
		return result
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("Serve returned %v", err)
		}
	})
	return stop
}

// dial opens a gRPC connection to a serving Server.
func dial(t *testing.T, s *Server, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()
	if creds == nil {
		creds = insecure.NewCredentials()
	}
	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("NewClient(%s): %v", s.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// memConn opens an in-memory connection to s.
func memConn(t *testing.T, s *Server) *grpc.ClientConn {
	t.Helper()
	conn, err := s.Conn()
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testCtx is a context bounded by testTimeout and the test's lifetime.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	t.Cleanup(cancel)
	return ctx
}

// createVM creates a MicroVM over conn and fails the test on error. With
// BootDelay zero it is CREATED on return.
func createVM(t *testing.T, conn grpc.ClientConnInterface, spec *types.MicroVMSpec) *types.MicroVM {
	t.Helper()
	if spec == nil {
		spec = &types.MicroVMSpec{Id: "vm", Namespace: "ns"}
	}
	resp, err := mvmv1.NewMicroVMClient(conn).CreateMicroVM(testCtx(t), &mvmv1.CreateMicroVMRequest{Microvm: spec})
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	return resp.GetMicrovm()
}

// execStreamClient is the client side of one ExecCommand exchange.
type execStreamClient = grpc.BidiStreamingClient[execv1.ExecCommandRequest, execv1.ExecCommandResponse]

// openExec opens an exec stream over conn.
func openExec(t *testing.T, conn grpc.ClientConnInterface) execStreamClient {
	t.Helper()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(testCtx(t))
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	return stream
}

// execResult is everything one exec exchange produced, in order.
type execResult struct {
	stdout, stderr strings.Builder
	errs           []string
	exit           *int32
	// err is the non-EOF error Recv ended with, nil after a clean EOF.
	err error
}

// sendStart sends the ExecStart, the stdin chunks and stdin_eof (when
// has_stdin), and half-closes. Send errors are ignored: the stream's
// status arrives on Recv.
func sendStart(stream execStreamClient, start *execv1.ExecStart, stdin ...string) {
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}})
	for _, chunk := range stdin {
		_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte(chunk)}})
	}
	if start.GetHasStdin() {
		_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_StdinEof{StdinEof: true}})
	}
	_ = stream.CloseSend()
}

// recvOne receives the next response into res and reports whether the
// stream is still open.
func recvOne(stream execStreamClient, res *execResult) bool {
	resp, err := stream.Recv()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			res.err = err
		}
		return false
	}
	switch p := resp.GetPayload().(type) {
	case *execv1.ExecCommandResponse_Stdout:
		res.stdout.Write(p.Stdout)
	case *execv1.ExecCommandResponse_Stderr:
		res.stderr.Write(p.Stderr)
	case *execv1.ExecCommandResponse_Error:
		res.errs = append(res.errs, p.Error)
	case *execv1.ExecCommandResponse_ExitCode:
		code := p.ExitCode
		res.exit = &code
	}
	return true
}

// drain receives until the stream ends.
func drain(stream execStreamClient, res *execResult) {
	for recvOne(stream, res) {
	}
}

// runExec performs one whole exchange over conn.
func runExec(t *testing.T, conn grpc.ClientConnInterface, start *execv1.ExecStart, stdin ...string) *execResult {
	t.Helper()
	stream := openExec(t, conn)
	sendStart(stream, start, stdin...)
	res := &execResult{}
	drain(stream, res)
	return res
}

// waitForStdout receives until stdout contains want, failing the test if
// the stream ends first.
func waitForStdout(t *testing.T, stream execStreamClient, res *execResult, want string) {
	t.Helper()
	for !strings.Contains(res.stdout.String(), want) {
		if !recvOne(stream, res) {
			t.Fatalf("stream ended before stdout contained %q: stdout=%q errs=%v err=%v", want, res.stdout.String(), res.errs, res.err)
		}
	}
}

// command builds an ExecStart for cmd in uid.
func command(uid, cmd string) *execv1.ExecStart {
	return &execv1.ExecStart{Uid: uid, Cmd: cmd, Shell: true}
}

// exitCode returns the exit code or fails.
func exitCode(t *testing.T, res *execResult) int32 {
	t.Helper()
	if res.exit == nil {
		t.Fatalf("no exit code received: stdout=%q stderr=%q errs=%v err=%v", res.stdout.String(), res.stderr.String(), res.errs, res.err)
	}
	return *res.exit
}
