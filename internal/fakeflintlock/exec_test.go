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
	"slices"
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/battery-operator/internal/clock"
)

//= docs/requirements/08-test-doubles.md#fake-flintlockd
//= type=test
//# The fake `flintlockd` SHALL let a test script the output and
//# exit status of an exec, and cut a stream before its exit status.

// TestScriptedExec scripts an exec's output and exit status, the guest
// agent's error-then-exit framing, and a stream cut before its exit status,
// both with Script and with an ExecFunc of the test's own, and checks what
// the client receives in each case.
func TestScriptedExec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fn         ExecFunc
		wantStdout string
		wantStderr string
		wantErrs   []string
		// wantExit nil means no exit status: the stream is cut.
		wantExit *int32
	}{
		{
			name:     "default: success with no output",
			wantExit: ptr(int32(0)),
		},
		{
			name:       "output and exit status",
			fn:         Script(Reply{Stdout: scriptOut, Stderr: scriptErr, ExitCode: 5}),
			wantStdout: scriptOut,
			wantStderr: scriptErr,
			wantExit:   ptr(int32(5)),
		},
		{
			name:       "error before the exit status",
			fn:         Script(Reply{Stdout: partialOut, Error: "guest agent failed", ExitCode: 1}),
			wantStdout: partialOut,
			wantErrs:   []string{"guest agent failed"},
			wantExit:   ptr(int32(1)),
		},
		{
			name:       "cut before the exit status",
			fn:         Script(Reply{Stdout: partialOut, ExitCode: 0, Cut: true}),
			wantStdout: partialOut,
		},
		{
			name: "an ExecFunc that cuts the stream itself",
			fn: func(_ context.Context, e *Exec) (int32, error) {
				_, _ = io.WriteString(e.Stdout, "a")
				_, _ = io.WriteString(e.Stdout, "b")
				return 0, ErrCutStream
			},
			wantStdout: "ab",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newTestServer(t, Config{})
			conn := memConn(t, s)
			uid := createVM(t, conn, nil).GetSpec().GetUid()
			s.SetExec(tt.fn)

			res := runExec(t, conn, command(uid, "anything"))
			if res.stdout.String() != tt.wantStdout || res.stderr.String() != tt.wantStderr {
				t.Errorf("output = %q/%q, want %q/%q", res.stdout.String(), res.stderr.String(), tt.wantStdout, tt.wantStderr)
			}
			if !slices.Equal(res.errs, tt.wantErrs) {
				t.Errorf("error messages = %q, want %q", res.errs, tt.wantErrs)
			}
			if tt.wantExit == nil {
				if res.exit != nil {
					t.Errorf("exit status %d received, want the stream cut first", *res.exit)
				}
				if status.Code(res.err) != codes.Unavailable {
					t.Errorf("cut stream ended with %v, want Unavailable", res.err)
				}
				return
			}
			if res.err != nil {
				t.Fatalf("stream error: %v", res.err)
			}
			if got := exitCode(t, res); got != *tt.wantExit {
				t.Errorf("exit status = %d, want %d", got, *tt.wantExit)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// Scripted output.
const (
	scriptOut  = "out"
	scriptErr  = "err"
	partialOut = "partial"
	catCmd     = "cat"
)

//= docs/requirements/08-test-doubles.md#fake-flintlockd
//= type=test
//# The fake `flintlockd` SHALL let a test script the output and
//# exit status of an exec, and cut a stream before its exit status.

// TestDropExecBeforeExitFault cuts every stream before its exit status
// while the fault is set, whatever the ExecFunc returns, and not once it is
// cleared.
func TestDropExecBeforeExitFault(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()
	s.SetExec(Script(Reply{Stdout: scriptOut, Stderr: scriptErr, ExitCode: 5}))
	s.SetFaults(Faults{DropExecBeforeExit: true})

	res := runExec(t, conn, command(uid, "anything"))
	if res.stdout.String() != scriptOut || res.stderr.String() != scriptErr {
		t.Errorf("output before the drop: stdout=%q stderr=%q", res.stdout.String(), res.stderr.String())
	}
	if res.exit != nil {
		t.Errorf("exit status %d received, want the stream dropped first", *res.exit)
	}
	if status.Code(res.err) != codes.Unavailable {
		t.Errorf("dropped stream ended with %v, want Unavailable", res.err)
	}

	s.SetFaults(Faults{})
	res = runExec(t, conn, command(uid, "anything"))
	if res.err != nil || exitCode(t, res) != 5 {
		t.Errorf("after clearing the fault: err=%v exit=%v", res.err, res.exit)
	}
}

// TestExecFuncSeesTheCommand checks what an ExecFunc is given: the start as
// the client sent it, the client's stdin up to stdin_eof, and nothing on
// stdin when the start did not ask for it; and that Execs records every
// accepted start in order.
func TestExecFuncSeesTheCommand(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()

	type seen struct {
		start *execv1.ExecStart
		stdin string
	}
	got := make(chan seen, 2)
	s.SetExec(func(_ context.Context, e *Exec) (int32, error) {
		in, err := io.ReadAll(e.Stdin)
		if err != nil {
			return 0, err
		}
		got <- seen{start: e.Start, stdin: string(in)}
		_, err = e.Stdout.Write(in)
		return 0, err
	})

	start := &execv1.ExecStart{
		Uid: uid, Cmd: catCmd, Args: []string{"-"}, Cwd: "/builds", Env: map[string]string{"CI": "yes"},
		User: "runner", TimeoutSeconds: 30, HasStdin: true,
	}
	res := runExec(t, conn, start, "hello ", "world")
	if res.err != nil || exitCode(t, res) != 0 || res.stdout.String() != "hello world" {
		t.Fatalf("exec with stdin: stdout=%q exit=%v err=%v", res.stdout.String(), res.exit, res.err)
	}
	first := <-got
	if first.stdin != "hello world" || first.start.GetCmd() != catCmd || first.start.GetCwd() != "/builds" ||
		first.start.GetEnv()["CI"] != "yes" || first.start.GetUser() != "runner" || first.start.GetTimeoutSeconds() != 30 {
		t.Errorf("ExecFunc saw %v with stdin %q", first.start, first.stdin)
	}

	// Without has_stdin, stdin messages are dropped.
	res = runExec(t, conn, command(uid, "no-stdin"), "ignored")
	if res.err != nil || exitCode(t, res) != 0 {
		t.Fatalf("exec without stdin: exit=%v err=%v", res.exit, res.err)
	}
	if second := <-got; second.stdin != "" {
		t.Errorf("stdin without has_stdin = %q, want empty", second.stdin)
	}

	execs := s.Execs()
	cmds := make([]string, 0, len(execs))
	for _, e := range execs {
		cmds = append(cmds, e.GetCmd())
	}
	if want := []string{catCmd, "no-stdin"}; !slices.Equal(cmds, want) {
		t.Errorf("Execs() = %v, want %v", cmds, want)
	}
}

// TestExecRejectsBadStreams: the flintlockd rules for the first message and
// the MicroVM's state. A refused start is not recorded.
func TestExecRejectsBadStreams(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(testEpoch)
	s := newTestServer(t, Config{BootDelay: time.Hour, Clock: clk})
	conn := memConn(t, s)
	pending := createVM(t, conn, nil).GetSpec().GetUid()

	tests := []struct {
		name string
		send func(s execStreamClient)
		want codes.Code
	}{
		{
			name: "stdin before start",
			send: func(s execStreamClient) {
				_ = s.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Stdin{Stdin: []byte("x")}})
			},
			want: codes.InvalidArgument,
		},
		{
			name: "start without uid",
			send: func(s execStreamClient) { sendStart(s, &execv1.ExecStart{Cmd: "true"}) },
			want: codes.InvalidArgument,
		},
		{
			name: "start without cmd or shell",
			send: func(s execStreamClient) { sendStart(s, &execv1.ExecStart{Uid: pending}) },
			want: codes.InvalidArgument,
		},
		{
			name: "unknown uid",
			send: func(s execStreamClient) { sendStart(s, command(missingUID, "true")) },
			want: codes.NotFound,
		},
		{
			name: "microvm still pending",
			send: func(s execStreamClient) { sendStart(s, command(pending, "true")) },
			want: codes.FailedPrecondition,
		},
		{
			name: "client half-closes without a start",
			send: func(s execStreamClient) { _ = s.CloseSend() },
			want: codes.Unknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := openExec(t, conn)
			tt.send(stream)
			res := &execResult{}
			drain(stream, res)
			if status.Code(res.err) != tt.want {
				t.Errorf("code = %s (%v), want %s", status.Code(res.err), res.err, tt.want)
			}
		})
	}
	if n := len(s.Execs()); n != 0 {
		t.Errorf("Execs() after only refused starts = %d, want none", n)
	}
}

// TestDeleteEndsRunningExec: DeleteMicroVM cancels the MicroVM's running
// command and the client is told why, as the guest agent would report a
// killed command.
func TestDeleteEndsRunningExec(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()
	s.SetExec(func(ctx context.Context, e *Exec) (int32, error) {
		_, _ = io.WriteString(e.Stdout, "started\n")
		<-ctx.Done()
		return 0, nil
	})

	stream := openExec(t, conn)
	sendStart(stream, command(uid, "sleep"))
	res := &execResult{}
	waitForStdout(t, stream, res, "started")

	if _, err := mvmv1.NewMicroVMClient(conn).DeleteMicroVM(testCtx(t), &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil {
		t.Fatalf("DeleteMicroVM: %v", err)
	}
	drain(stream, res)
	if res.err != nil {
		t.Fatalf("stream error: %v", res.err)
	}
	if !slices.Equal(res.errs, []string{errVMDeleted.Error()}) {
		t.Errorf("error messages = %q, want one about the deletion", res.errs)
	}
	if got := exitCode(t, res); got != exitKilled {
		t.Errorf("exit status = %d, want %d", got, exitKilled)
	}
}

// TestExecCancelledByClient: a client that goes away cancels the command.
func TestExecCancelledByClient(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()
	ended := make(chan error, 1)
	s.SetExec(func(ctx context.Context, e *Exec) (int32, error) {
		_, _ = io.WriteString(e.Stdout, "started\n")
		<-ctx.Done()
		ended <- ctx.Err()
		return 0, nil
	})

	ctx, cancel := context.WithCancel(testCtx(t))
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}
	sendStart(stream, command(uid, "sleep"))
	res := &execResult{}
	waitForStdout(t, stream, res, "started")
	cancel()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the command's context ended with %v, want canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the command was not cancelled when its client went away")
	}
}

// TestCloseEndsExecs: Close ends a stream that never sent its start and a
// command that is still running, and waits for both.
func TestCloseEndsExecs(t *testing.T) {
	t.Parallel()
	s := New(Config{})
	conn := memConn(t, s)
	uid := createVM(t, conn, nil).GetSpec().GetUid()
	s.SetExec(func(ctx context.Context, e *Exec) (int32, error) {
		_, _ = io.WriteString(e.Stdout, "started\n")
		<-ctx.Done()
		return 0, nil
	})

	idle := openExec(t, conn)
	running := openExec(t, conn)
	sendStart(running, command(uid, "sleep"))
	res := &execResult{}
	waitForStdout(t, running, res, "started")

	closed := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(testTimeout):
		t.Fatal("Close did not return with a running command and an idle stream")
	}
	for name, stream := range map[string]execStreamClient{"idle": idle, "running": running} {
		res := &execResult{}
		drain(stream, res)
		if res.exit != nil || status.Code(res.err) != codes.Unavailable {
			t.Errorf("%s stream after Close: exit=%v err=%v, want Unavailable and no exit status", name, res.exit, res.err)
		}
	}
}
