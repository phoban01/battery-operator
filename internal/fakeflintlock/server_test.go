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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/battery-operator/internal/clock"
)

//= docs/requirements/08-test-doubles.md#fake-flintlockd
//= type=test
//# The fake `flintlockd` SHALL serve flintlock's MicroVM and
//# `MicroVMExec` services over gRPC, with MicroVMs that exist only in memory.

// TestServesMicroVMAndMicroVMExec drives every MicroVM RPC and an
// ExecCommand through the generated gRPC clients, over TCP and over the
// in-memory connection, then stops the Server and checks that its MicroVMs
// went with it.
func TestServesMicroVMAndMicroVMExec(t *testing.T) {
	t.Parallel()
	transports := map[string]func(t *testing.T, s *Server) (grpc.ClientConnInterface, func() error){
		"tcp": func(t *testing.T, s *Server) (grpc.ClientConnInterface, func() error) {
			stop := serve(t, s)
			if s.Addr() == "" {
				t.Fatal("Addr empty while serving")
			}
			return dial(t, s, nil), stop
		},
		"memory": func(t *testing.T, s *Server) (grpc.ClientConnInterface, func() error) {
			return memConn(t, s), s.Close
		},
	}
	for name, connect := range transports {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, Config{Name: "h1", Version: "v0.14.0", ExecEnabled: true})
			conn, stop := connect(t, s)
			vms := mvmv1.NewMicroVMClient(conn)
			ctx := testCtx(t)

			info, err := vms.ServerInfo(ctx, &emptypb.Empty{})
			if err != nil {
				t.Fatalf("ServerInfo: %v", err)
			}
			if info.GetVersion().GetVersion() != "v0.14.0" || !info.GetExec().GetEnabled() || info.GetExec().GetAddress() != s.Addr() {
				t.Errorf("ServerInfo = %v, want version v0.14.0 and exec enabled at %q", info, s.Addr())
			}

			created, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{
				Microvm: &types.MicroVMSpec{
					Id: "vm1", Namespace: "ns",
					Interfaces: []*types.NetworkInterface{{DeviceId: "net0", Type: types.NetworkInterface_TAP}},
				},
			})
			if err != nil {
				t.Fatalf("CreateMicroVM: %v", err)
			}
			uid := created.GetMicrovm().GetSpec().GetUid()
			if uid == "" {
				t.Fatal("CreateMicroVM returned no uid")
			}
			if got := created.GetMicrovm().GetStatus().GetState(); got != types.MicroVMStatus_CREATED {
				t.Errorf("state with zero BootDelay = %s, want CREATED", got)
			}
			if _, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("CreateMicroVM(no spec) code = %s, want InvalidArgument", status.Code(err))
			}

			got, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
			if err != nil {
				t.Fatalf("GetMicroVM: %v", err)
			}
			st := got.GetMicrovm().GetStatus()
			if got.GetMicrovm().GetSpec().GetId() != "vm1" || st.GetVsockPath() == "" || st.GetNetworkInterfaces()["net0"].GetHostDeviceName() == "" {
				t.Errorf("GetMicroVM = %v, want id vm1 with a vsock_path and a net0 interface", got.GetMicrovm())
			}
			if _, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: missingUID}); status.Code(err) != codes.NotFound {
				t.Errorf("GetMicroVM(missing) code = %s, want NotFound", status.Code(err))
			}

			checkListing(t, vms, uid)

			s.SetExec(Script(Reply{Stdout: "hello", ExitCode: 7}))
			res := runExec(t, conn, command(uid, "greet"))
			if res.err != nil || res.stdout.String() != "hello" || exitCode(t, res) != 7 {
				t.Errorf("ExecCommand: stdout=%q exit=%v err=%v", res.stdout.String(), res.exit, res.err)
			}

			if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil {
				t.Fatalf("DeleteMicroVM: %v", err)
			}
			if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: uid}); status.Code(err) != codes.NotFound {
				t.Errorf("second DeleteMicroVM code = %s, want NotFound", status.Code(err))
			}

			// The MicroVMs live only in the Server's memory: nothing of them
			// outlives it.
			createVM(t, conn, nil)
			if n := len(s.MicroVMs()); n != 1 {
				t.Fatalf("MicroVMs() = %d, want 1", n)
			}
			if err := stop(); err != nil {
				t.Errorf("stopping the server: %v", err)
			}
			if n := len(s.MicroVMs()); n != 0 {
				t.Errorf("MicroVMs() after the Server stopped = %d, want none", n)
			}
			if _, err := vms.ServerInfo(ctx, &emptypb.Empty{}); err == nil {
				t.Error("ServerInfo succeeded after the Server stopped")
			}
		})
	}
}

// checkListing checks that ListMicroVMs and ListMicroVMsStream return the
// one MicroVM uid in namespace ns and nothing elsewhere.
func checkListing(t *testing.T, vms mvmv1.MicroVMClient, uid string) {
	t.Helper()
	ctx := testCtx(t)
	list, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "ns"})
	if err != nil {
		t.Fatalf("ListMicroVMs: %v", err)
	}
	if len(list.GetMicrovm()) != 1 || list.GetMicrovm()[0].GetSpec().GetUid() != uid {
		t.Errorf("ListMicroVMs(ns) = %v, want the one MicroVM", list.GetMicrovm())
	}
	other, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "other"})
	if err != nil || len(other.GetMicrovm()) != 0 {
		t.Errorf("ListMicroVMs(other) = %v, %v; want empty", other.GetMicrovm(), err)
	}

	stream, err := vms.ListMicroVMsStream(ctx, &mvmv1.ListMicroVMsRequest{Namespace: "ns"})
	if err != nil {
		t.Fatalf("ListMicroVMsStream: %v", err)
	}
	var streamed int
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ListMicroVMsStream Recv: %v", err)
		}
		if msg.GetMicrovm().GetSpec().GetUid() != uid {
			t.Errorf("streamed uid %q, want %q", msg.GetMicrovm().GetSpec().GetUid(), uid)
		}
		streamed++
	}
	if streamed != 1 {
		t.Errorf("ListMicroVMsStream sent %d messages, want 1", streamed)
	}
}

// TestServeTwiceAndAfterClose: Serve is one-shot and refuses a closed
// Server, and a closed Server answers UNAVAILABLE in memory.
func TestServeTwiceAndAfterClose(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	serve(t, s)
	if err := s.Serve(context.Background()); err == nil {
		t.Error("second Serve succeeded")
	}

	closed := newTestServer(t, Config{})
	conn := memConn(t, closed)
	_ = closed.Close()
	if err := closed.Serve(context.Background()); err == nil {
		t.Error("Serve after Close succeeded")
	}
	if _, err := mvmv1.NewMicroVMClient(conn).ServerInfo(testCtx(t), &emptypb.Empty{}); status.Code(err) != codes.Unavailable {
		t.Errorf("ServerInfo on a closed Server: code = %s, want Unavailable", status.Code(err))
	}
	if _, err := closed.CreateMicroVM(&types.MicroVMSpec{}); status.Code(err) != codes.Unavailable {
		t.Errorf("CreateMicroVM on a closed Server: code = %s, want Unavailable", status.Code(err))
	}
}

// TestTLS: Serve listens with TLS from Config.TLS, verifiable against the
// test CA, and requires a client certificate when ClientCAFile is set,
// which is how battery reaches flintlockd.
func TestTLS(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#fake-flintlockd
	//= type=test
	//# Where a test gives it a client certificate authority, the fake
	//# `flintlockd` SHALL serve over TLS and refuse a client whose certificate
	//# that authority did not sign.
	t.Parallel()
	certs, err := WriteTestCerts(t.TempDir(), "h1")
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	// A second, unrelated CA, whose client certificate the server must refuse.
	other, err := WriteTestCerts(t.TempDir(), "h1")
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	foreignCert, err := tls.LoadX509KeyPair(other.ClientCertFile, other.ClientKeyFile)
	if err != nil {
		t.Fatalf("loading foreign client cert: %v", err)
	}
	caPEM, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA file has no certificate")
	}
	clientCert, err := tls.LoadX509KeyPair(certs.ClientCertFile, certs.ClientKeyFile)
	if err != nil {
		t.Fatalf("loading client cert: %v", err)
	}
	for _, name := range []string{certs.CAFile, certs.ServerCertFile, certs.ServerKeyFile, certs.ClientCertFile, certs.ClientKeyFile} {
		if filepath.Dir(name) != certs.Dir {
			t.Errorf("%s not under %s", name, certs.Dir)
		}
	}

	tests := []struct {
		name   string
		mutual bool
		creds  credentials.TransportCredentials
		wantOK bool
	}{
		{name: "server tls, verifying client", creds: credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}), wantOK: true},
		{name: "server tls, plaintext client", creds: nil, wantOK: false},
		{name: "server tls, wrong roots", creds: credentials.NewTLS(&tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}), wantOK: false},
		{name: "mutual tls, client with cert", mutual: true, creds: credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS12}), wantOK: true},
		{name: "mutual tls, client without cert", mutual: true, creds: credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}), wantOK: false},
		{name: "mutual tls, client cert from another CA", mutual: true, creds: credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{foreignCert}, MinVersion: tls.VersionTLS12}), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, Config{Name: "h1", TLS: certs.ServerTLS(tt.mutual)})
			serve(t, s)
			conn := dial(t, s, tt.creds)
			ctx, cancel := context.WithTimeout(testCtx(t), 5*time.Second)
			defer cancel()
			_, err := mvmv1.NewMicroVMClient(conn).ServerInfo(ctx, &emptypb.Empty{})
			if (err == nil) != tt.wantOK {
				t.Errorf("ServerInfo err = %v, want ok=%v", err, tt.wantOK)
			}
		})
	}

	t.Run("bad certificate files", func(t *testing.T) {
		t.Parallel()
		s := newTestServer(t, Config{TLS: &TLS{CertFile: "/nonexistent", KeyFile: "/nonexistent"}})
		if err := s.Serve(context.Background()); err == nil {
			t.Error("Serve with missing certificate files succeeded")
		}
	})
}

// TestServerInfo checks the configured version, exec flag and uptime.
func TestServerInfo(t *testing.T) {
	t.Parallel()
	for _, exec := range []bool{false, true} {
		clk := clock.NewFake(testEpoch)
		s := newTestServer(t, Config{Name: "h1", Version: "v0.99.0", ExecEnabled: exec, Clock: clk})
		serve(t, s)
		clk.Advance(90 * time.Second)

		resp, err := mvmv1.NewMicroVMClient(dial(t, s, nil)).ServerInfo(testCtx(t), &emptypb.Empty{})
		if err != nil {
			t.Fatalf("ServerInfo: %v", err)
		}
		if resp.GetVersion().GetVersion() != "v0.99.0" {
			t.Errorf("version = %q, want v0.99.0", resp.GetVersion().GetVersion())
		}
		wantAddr := ""
		if exec {
			wantAddr = s.Addr()
		}
		if resp.GetExec().GetEnabled() != exec || resp.GetExec().GetAddress() != wantAddr {
			t.Errorf("exec = %v, want enabled=%v at %q", resp.GetExec(), exec, wantAddr)
		}
		if resp.GetSshProxy().GetEnabled() {
			t.Errorf("ssh_proxy = %v, want disabled", resp.GetSshProxy())
		}
		if got := resp.GetUptime().AsDuration(); got != 90*time.Second {
			t.Errorf("uptime = %v, want 90s", got)
		}
	}
}

// TestUnresponsive: with the fault on, RPCs hang until the caller's
// deadline, in-flight exec streams stall, and clearing the fault lets
// everything resume.
func TestUnresponsive(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()
	vms := mvmv1.NewMicroVMClient(conn)

	// An exec that writes, waits for a line of stdin, echoes it and exits.
	s.SetExec(func(_ context.Context, e *Exec) (int32, error) {
		if _, err := io.WriteString(e.Stdout, "first\n"); err != nil {
			return 0, err
		}
		line := make([]byte, len("second\n"))
		if _, err := io.ReadFull(e.Stdin, line); err != nil {
			return 0, err
		}
		if _, err := e.Stdout.Write(line); err != nil {
			return 0, err
		}
		return 0, nil
	})
	stream := openExec(t, conn)
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{
		Start: &execv1.ExecStart{Uid: uid, Cmd: "echo-line", HasStdin: true},
	}})
	res := &execResult{}
	waitForStdout(t, stream, res, "first")

	s.SetFaults(Faults{Unresponsive: true})

	// New unary RPCs time out at the caller's deadline rather than fail.
	ctx, cancel := context.WithTimeout(testCtx(t), 100*time.Millisecond)
	_, err := vms.ServerInfo(ctx, &emptypb.Empty{})
	cancel()
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("ServerInfo while unresponsive: code = %s (%v), want DeadlineExceeded", status.Code(err), err)
	}

	// The in-flight exec produces output but the Server does not forward it.
	_ = stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte("second\n")}})
	got := make(chan bool, 1)
	go func() { got <- recvOne(stream, res) }()
	select {
	case <-got:
		t.Fatalf("stream delivered %q while the Server was unresponsive", res.stdout.String())
	case <-time.After(100 * time.Millisecond):
	}

	// Clearing the fault wakes the held send and later RPCs succeed.
	s.SetFaults(Faults{})
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("stream ended after recovery: err=%v", res.err)
		}
	case <-time.After(testTimeout):
		t.Fatal("stream did not resume after the fault was cleared")
	}
	if res.stdout.String() != "first\nsecond\n" {
		t.Errorf("stdout after recovery = %q, want %q", res.stdout.String(), "first\nsecond\n")
	}
	_ = stream.CloseSend()
	drain(stream, res)
	if res.err != nil || exitCode(t, res) != 0 {
		t.Errorf("stream end after recovery: err=%v exit=%v", res.err, res.exit)
	}
	if _, err := vms.ServerInfo(testCtx(t), &emptypb.Empty{}); err != nil {
		t.Errorf("ServerInfo after recovery: %v", err)
	}

	// Closing the Server while a call is held by the fault fails it.
	s.SetFaults(Faults{Unresponsive: true})
	held := make(chan error, 1)
	go func() {
		_, err := vms.GetMicroVM(testCtx(t), &mvmv1.GetMicroVMRequest{Uid: uid})
		held <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = s.Close()
	select {
	case err := <-held:
		if status.Code(err) != codes.Unavailable {
			t.Errorf("held call after Close = %v, want Unavailable", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("held call did not return after Close")
	}
}
