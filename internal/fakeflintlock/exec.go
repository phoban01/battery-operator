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
	"fmt"
	"io"
	"strings"
	"sync"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrCutStream is what an ExecFunc returns to have the stream cut after
// its output and before its exit status. The client sees UNAVAILABLE and
// no exit_code, as it would if the Host died under the command.
var ErrCutStream = errors.New("fake flintlockd: exec stream cut before its exit status")

// errVMDeleted is the cause an exec's context is cancelled with when its
// MicroVM is deleted under it.
var errVMDeleted = errors.New("microvm deleted while command was running")

// exitKilled is the exit status reported for a command whose MicroVM was
// deleted under it: 128 plus SIGKILL, as a shell reports a killed child.
const exitKilled = 137

// Exec is one command as an ExecFunc sees it.
type Exec struct {
	// Start is the ExecStart that opened the stream: uid, cmd, args, cwd,
	// env, user, timeout_seconds and the stdin flag, as the client sent
	// them.
	Start *execv1.ExecStart
	// Stdin is the client's standard input, which ends at stdin_eof or when
	// the client half-closes. It is empty unless the start set has_stdin.
	Stdin io.Reader
	// Stdout and Stderr send each Write to the client as one chunk. A
	// Write fails once the client has gone.
	Stdout io.Writer
	Stderr io.Writer
}

// ExecFunc runs one command. It returns the exit status; a non-nil error
// is sent as the guest agent's error message ahead of the exit status, and
// ErrCutStream cuts the stream before the exit status instead. ctx ends
// when the client goes away, the MicroVM is deleted or the Server is
// closed, and an ExecFunc has to return then: Close waits for it.
type ExecFunc func(ctx context.Context, e *Exec) (exitCode int32, err error)

// Reply is a canned result for Script.
type Reply struct {
	// Stdout and Stderr are written in that order, each as one chunk when
	// not empty.
	Stdout, Stderr string
	// ExitCode is the exit status.
	ExitCode int32
	// Error, when set, is sent as the guest agent's error message before
	// the exit status.
	Error string
	// Cut cuts the stream after the output, before the exit status.
	Cut bool
}

//= docs/requirements/08-test-doubles.md#fake-flintlockd
//# The fake `flintlockd` SHALL let a test script the output and
//# exit status of an exec, and cut a stream before its exit status.

// Script returns an ExecFunc that answers every command with r.
func Script(r Reply) ExecFunc {
	return func(_ context.Context, e *Exec) (int32, error) {
		if r.Stdout != "" {
			if _, err := io.WriteString(e.Stdout, r.Stdout); err != nil {
				return 0, err
			}
		}
		if r.Stderr != "" {
			if _, err := io.WriteString(e.Stderr, r.Stderr); err != nil {
				return 0, err
			}
		}
		switch {
		case r.Cut:
			return 0, ErrCutStream
		case r.Error != "":
			return r.ExitCode, errors.New(r.Error)
		}
		return r.ExitCode, nil
	}
}

// succeed is the ExecFunc of a Server nobody scripted: every command exits
// zero with no output.
func succeed(context.Context, *Exec) (int32, error) { return 0, nil }

// SetExec sets the ExecFunc that runs every command from now on; nil puts
// back the default, which exits zero with no output. A command already
// running keeps the function it started with.
func (s *Server) SetExec(fn ExecFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exec = fn
}

// Execs returns a copy of every ExecStart the Server accepted, in order.
// A start refused for its form or for its MicroVM is not recorded.
func (s *Server) Execs() []*execv1.ExecStart {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*execv1.ExecStart, 0, len(s.execs))
	for _, e := range s.execs {
		out = append(out, proto.CloneOf(e))
	}
	return out
}

// accepted records start and returns the ExecFunc to run it with.
func (s *Server) accepted(start *execv1.ExecStart) ExecFunc {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs = append(s.execs, proto.CloneOf(start))
	if s.exec == nil {
		return succeed
	}
	return s.exec
}

// execStream is the server side of one ExecCommand exchange.
type execStream interface {
	Context() context.Context
	Send(*execv1.ExecCommandResponse) error
	Recv() (*execv1.ExecCommandRequest, error)
}

// execCommand serves one ExecCommand exchange: the first message has to be
// an ExecStart naming a CREATED MicroVM; the command is then handed to the
// Server's ExecFunc, with stdin relayed from the client until stdin_eof or
// half-close and each write to stdout or stderr sent as a chunk, and the
// exchange ends as the ExecFunc's result says. The framing rules and status
// codes are flintlockd's: InvalidArgument for a bad first message, NotFound
// for an unknown uid, FailedPrecondition for a MicroVM that is not CREATED.
func (s *Server) execCommand(stream execStream) error {
	// ctx ends when the client goes away, the Server closes or the MicroVM
	// is deleted. It is wired to the Server's lifetime before the first
	// message is waited for, because a client that opens a stream and says
	// nothing would otherwise hold this handler where Close cannot end it.
	ctx, cancel := context.WithCancelCause(stream.Context())
	defer cancel(nil)
	defer context.AfterFunc(s.ctx, func() { cancel(errClosed) })()

	// The client's messages are relayed onto a channel so that waiting for
	// one can be interrupted. Closing done ends the relay with the handler.
	done := make(chan struct{})
	defer close(done)
	requests := recvLoop(stream, done)

	var first *execv1.ExecCommandRequest
	select {
	case req := <-requests:
		if req.err != nil {
			return fmt.Errorf("receiving exec start message: %w", req.err)
		}
		first = req.msg
	case <-ctx.Done():
		return endedStatus(ctx)
	}

	start := first.GetStart()
	if start == nil || start.GetUid() == "" || (start.GetCmd() == "" && !start.GetShell()) {
		return status.Error(codes.InvalidArgument, "first message must be a start message with uid and cmd set")
	}
	detach, err := s.attachExec(start.GetUid(), cancel)
	if err != nil {
		return err
	}
	defer detach()
	fn := s.accepted(start)

	run := &execRun{s: s, ctx: ctx, stream: stream}
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinR.CloseWithError(io.ErrClosedPipe) }()
	go pumpStdin(requests, stdinW, start.GetHasStdin())
	var stdin io.Reader = strings.NewReader("")
	if start.GetHasStdin() {
		stdin = stdinR
	}

	code, err := fn(ctx, &Exec{
		Start:  proto.CloneOf(start),
		Stdin:  stdin,
		Stdout: &chunkWriter{run: run, stderr: false},
		Stderr: &chunkWriter{run: run, stderr: true},
	})

	// The client is gone or the Server is closing: nobody is listening, so
	// end the stream with the cause rather than an exit status.
	if ctx.Err() != nil && !errors.Is(context.Cause(ctx), errVMDeleted) {
		return endedStatus(ctx)
	}
	switch {
	case errors.Is(context.Cause(ctx), errVMDeleted):
		return run.finish(errVMDeleted.Error(), exitKilled)
	case errors.Is(err, ErrCutStream), s.Faults().DropExecBeforeExit:
		return status.Error(codes.Unavailable, "fake flintlockd: exec stream dropped before exit code")
	case err != nil:
		return run.finish(err.Error(), code)
	}
	return run.exit(code)
}

// endedStatus is the status for a stream whose context has ended before
// its exit status could be sent.
func endedStatus(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), errClosed) {
		return errClosedStatus()
	}
	return status.FromContextError(ctx.Err()).Err()
}

// execRequest is one message from the client, or the error that ended the
// stream.
type execRequest struct {
	msg *execv1.ExecCommandRequest
	err error
}

// recvLoop relays the client's messages onto a channel until the stream
// ends or done is closed.
func recvLoop(stream execStream, done <-chan struct{}) <-chan execRequest {
	ch := make(chan execRequest)
	go func() {
		defer close(ch)
		for {
			msg, err := stream.Recv()
			select {
			case ch <- execRequest{msg: msg, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// pumpStdin relays stdin messages into w until stdin_eof, half-close or
// the end of the stream, and then closes w. Without has_stdin the messages
// are read and dropped, so that a chatty client never blocks on flow
// control. A write fails once the handler has finished, which ends it.
func pumpStdin(requests <-chan execRequest, w *io.PipeWriter, hasStdin bool) {
	defer func() { _ = w.Close() }()
	for req := range requests {
		if req.err != nil {
			return
		}
		switch p := req.msg.GetPayload().(type) {
		case *execv1.ExecCommandRequest_Stdin:
			if !hasStdin {
				continue
			}
			if _, err := w.Write(p.Stdin); err != nil {
				return
			}
		case *execv1.ExecCommandRequest_StdinEof:
			if p.StdinEof && hasStdin {
				_ = w.Close()
			}
		}
	}
}

// execRun is the sending side of one running command.
type execRun struct {
	s      *Server
	ctx    context.Context
	stream execStream

	// sendMu serialises Send, which the stream does not allow from more
	// than one goroutine.
	sendMu  sync.Mutex
	sendErr error
}

// chunkWriter sends each write as one stdout or stderr chunk, copying the
// bytes because the stream may retain the message.
type chunkWriter struct {
	run    *execRun
	stderr bool
}

// Write implements io.Writer.
func (w *chunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	data := append([]byte(nil), p...)
	resp := &execv1.ExecCommandResponse{}
	if w.stderr {
		resp.Payload = &execv1.ExecCommandResponse_Stderr{Stderr: data}
	} else {
		resp.Payload = &execv1.ExecCommandResponse_Stdout{Stdout: data}
	}
	if err := w.run.send(resp); err != nil {
		return 0, err
	}
	return len(p), nil
}

// send writes one response, held first while the Server is Unresponsive.
// After the first failure every send returns the same error.
func (r *execRun) send(resp *execv1.ExecCommandResponse) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.sendErr != nil {
		return r.sendErr
	}
	if err := r.s.awaitResponsive(r.ctx); err != nil {
		r.sendErr = err
		return err
	}
	if err := r.stream.Send(resp); err != nil {
		r.sendErr = err
		return err
	}
	return nil
}

// finish sends an error message followed by the exit status, the guest
// agent's way of reporting a failure of its own.
func (r *execRun) finish(msg string, code int32) error {
	if err := r.send(&execv1.ExecCommandResponse{
		Payload: &execv1.ExecCommandResponse_Error{Error: msg},
	}); err != nil {
		return fmt.Errorf("sending exec error: %w", err)
	}
	return r.exit(code)
}

// exit ends the stream with its exit status.
func (r *execRun) exit(code int32) error {
	if err := r.send(&execv1.ExecCommandResponse{
		Payload: &execv1.ExecCommandResponse_ExitCode{ExitCode: code},
	}); err != nil {
		return fmt.Errorf("sending exit code: %w", err)
	}
	return nil
}
