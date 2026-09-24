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
	"fmt"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// TestExecRelaysACommand runs a command through the agent whose standard
// input is far larger than one chunk, with a working directory and an
// environment. flintlockd sees the ExecStart as the caller sent it and the
// whole of standard input; its writes to both output streams and its exit
// status of its own choosing each arrive. Every exit status arrives as it
// was sent. A caller that does not trust the agent's certificate authority
// never reaches flintlockd.
func TestExecRelaysACommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t, hostOptions{})
	f.bind("claim")

	stdinSeen := make(chan string, 1)
	f.host.Fake.SetExec(func(_ context.Context, e *fakeflintlock.Exec) (int32, error) {
		in, err := io.ReadAll(e.Stdin)
		if err != nil {
			return 0, err
		}
		stdinSeen <- string(in)
		lines := strings.Split(strings.TrimSpace(string(in)), "\n")
		_, _ = fmt.Fprintf(e.Stdout, "%s\n%s\n%s\n", e.Start.GetCwd(), e.Start.GetEnv()["STAGE"], lines[len(lines)-1])
		_, _ = io.WriteString(e.Stderr, "to-stderr\n")
		return 7, nil
	})

	var script strings.Builder
	for script.Len() < 256*1024 {
		script.WriteString(": padding the input past one stdin chunk\n")
	}
	script.WriteString("last-line\n")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	start := f.start("sh")
	start.Cwd, start.Env = "/builds/project", map[string]string{"STAGE": "build"}
	r := exchange(ctx, f.rawExec(f.holder.Token), start, script.String(), nil)
	if r.err != nil || !r.gotExit || r.exitCode != 7 {
		t.Fatalf("exec = (exit %d, sent %v, %v), want exit 7\n%s", r.exitCode, r.gotExit, r.err, f.host.Log())
	}
	if r.stdout != "/builds/project\nbuild\nlast-line\n" {
		t.Errorf("stdout = %q, want the directory, the variable and the last line", r.stdout)
	}
	if r.stderr != "to-stderr\n" {
		t.Errorf("stderr = %q", r.stderr)
	}
	if seen := <-stdinSeen; seen != script.String() {
		t.Errorf("flintlockd saw %d bytes of standard input, want %d", len(seen), script.Len())
	}
	execs := f.host.Fake.Execs()
	if got := execs[len(execs)-1]; got.GetCwd() != "/builds/project" || !maps.Equal(got.GetEnv(), start.Env) || !got.GetHasStdin() {
		t.Errorf("flintlockd saw the start %v, want the caller's", got)
	}

	for _, code := range []int32{0, 1, 2, 42, 255} {
		f.host.Fake.SetExec(fakeflintlock.Script(fakeflintlock.Reply{ExitCode: code}))
		r := exchange(ctx, f.rawExec(f.holder.Token), f.start(fmt.Sprint("exit-", code)), "", nil)
		if r.err != nil || !r.gotExit || r.exitCode != code {
			t.Errorf("exit %d: exec = (exit %d, sent %v, %v)", code, r.exitCode, r.gotExit, r.err)
		}
	}

	// A certificate authority that did not issue the agent's certificate
	// fails the handshake: nothing reaches flintlockd.
	other, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	untrusted := execv1.NewMicroVMExecClient(conn(t, f.host.Address, other.CAFile, f.holder.Token))
	if r := exchange(ctx, untrusted, f.start("untrusted"), "", nil); r.err == nil || r.gotExit {
		t.Errorf("exec against an agent the caller's authority did not certify = (sent %v, %v), want a failure", r.gotExit, r.err)
	}
	if f.ran("untrusted") {
		t.Error("a command ran over a connection whose certificate was not verified")
	}
}

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// TestExecStreamsOutputAsItIsProduced has flintlockd write a line and then
// wait for the caller to have received it before it writes the next and
// exits. A relay that held output back until the end would never finish.
func TestExecStreamsOutputAsItIsProduced(t *testing.T) {
	t.Parallel()
	f := newFixture(t, hostOptions{})
	f.bind("claim")
	seen := make(chan struct{})
	f.host.Fake.SetExec(func(ctx context.Context, e *fakeflintlock.Exec) (int32, error) {
		_, _ = io.WriteString(e.Stdout, "first\n")
		select {
		case <-seen:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		_, _ = io.WriteString(e.Stdout, "second\n")
		return 0, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	var once sync.Once
	r := exchange(ctx, f.rawExec(f.holder.Token), f.start("stream"), "", func(chunk string) {
		if strings.Contains(chunk, "first") {
			once.Do(func() { close(seen) })
		}
	})
	if r.err != nil || !r.gotExit || r.exitCode != 0 || r.stdout != "first\nsecond\n" {
		t.Errorf("exec = (exit %d, sent %v, %v, %q), want both lines and exit 0", r.exitCode, r.gotExit, r.err, r.stdout)
	}
}

// blockUntilCancelled is an ExecFunc that writes "started", then waits for
// its stream to be cancelled and reports that it was on cancelled.
func blockUntilCancelled(cancelled chan<- struct{}) fakeflintlock.ExecFunc {
	return func(ctx context.Context, e *fakeflintlock.Exec) (int32, error) {
		_, _ = io.WriteString(e.Stdout, "started\n")
		<-ctx.Done()
		cancelled <- struct{}{}
		return 0, ctx.Err()
	}
}

// runLong runs a command that blocks until cancelled, in the background,
// and returns a channel closed once it has started and one that carries
// how the exchange ended.
func runLong(ctx context.Context, client execv1.MicroVMExecClient, start *execv1.ExecStart) (<-chan struct{}, <-chan result) {
	started := make(chan struct{})
	done := make(chan result, 1)
	var once sync.Once
	go func() {
		done <- exchange(ctx, client, start, "", func(chunk string) {
			if strings.Contains(chunk, "started") {
				once.Do(func() { close(started) })
			}
		})
	}()
	return started, done
}

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// TestExecCancellationAndTimeout stops a command that would run forever by
// cancelling the caller's context and by its deadline passing. The
// exchange ends at once with the context's own status and no exit status,
// and flintlockd's stream is cancelled, which is what stops the command in
// the guest.
func TestExecCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t, hostOptions{})
	f.bind("claim")
	cancelled := make(chan struct{}, 1)
	f.host.Fake.SetExec(blockUntilCancelled(cancelled))
	client := f.rawExec(f.holder.Token)

	for _, how := range []string{"cancelled", "deadline"} {
		t.Run(how, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			want := codes.Canceled
			if how == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
				want = codes.DeadlineExceeded
			}
			defer cancel()
			started, done := runLong(ctx, client, f.start("run-"+how))
			select {
			case <-started:
			case <-time.After(testTimeout):
				t.Fatal("the command never started")
			}
			if how == "cancelled" {
				cancel()
			}
			select {
			case r := <-done:
				if status.Code(r.err) != want || r.gotExit {
					t.Errorf("exec = (sent %v, %v), want %v and no exit status", r.gotExit, r.err, want)
				}
			case <-time.After(testTimeout):
				t.Fatal("the exchange did not end after its context did")
			}
			select {
			case <-cancelled:
			case <-time.After(testTimeout):
				t.Fatal("flintlockd's stream was not cancelled")
			}
		})
	}
}

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# The Exec Agent SHALL relay a request's streams to
//# `MicroVMExec.ExecCommand` on the local `flintlockd`, and SHALL end every
//# response with an exit status frame, sent only after `flintlockd` has
//# reported the command's exit.

// TestACutResponseIsNeverASuccess cuts a running command's session in each
// way it can be cut -- flintlockd dropping its stream before the exit code,
// the Exec Agent restarting, and the connection between the caller and the
// agent being cut -- and each ends as a failure with no exit status. After
// the restart the agent serves again.
func TestACutResponseIsNeverASuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t, hostOptions{})
	f.bind("claim")

	t.Run("flintlockd drops the stream before the exit code", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		f.host.Fake.SetExec(fakeflintlock.Script(fakeflintlock.Reply{Stdout: "partial\n", Cut: true}))
		r := exchange(ctx, f.rawExec(f.holder.Token), f.start("cut"), "", nil)
		if status.Code(r.err) != codes.Unavailable || r.gotExit {
			t.Errorf("exec = (sent %v, %v), want unavailable and no exit status", r.gotExit, r.err)
		}
		if r.stdout != "partial\n" {
			t.Errorf("stdout = %q, want what flintlockd sent before the cut", r.stdout)
		}

		f.host.Fake.SetExec(nil)
		f.host.Fake.SetFaults(fakeflintlock.Faults{DropExecBeforeExit: true})
		defer f.host.Fake.SetFaults(fakeflintlock.Faults{})
		r = exchange(ctx, f.rawExec(f.holder.Token), f.start("dropped"), "", nil)
		if status.Code(r.err) != codes.Unavailable || r.gotExit {
			t.Errorf("exec = (sent %v, %v), want unavailable and no exit status", r.gotExit, r.err)
		}
	})

	t.Run("the exec agent restarts", func(t *testing.T) {
		cancelled := make(chan struct{}, 1)
		f.host.Fake.SetExec(blockUntilCancelled(cancelled))
		ctx, cancel := context.WithTimeout(context.Background(), 2*testTimeout)
		defer cancel()
		client := f.rawExec(f.holder.Token)
		started, done := runLong(ctx, client, f.start("restart"))
		select {
		case <-started:
		case <-time.After(testTimeout):
			t.Fatal("the command never started")
		}
		f.host.StopAgent()
		select {
		case r := <-done:
			if r.err == nil || r.gotExit {
				t.Errorf("exec across an agent restart = (sent %v, %v), want a failure and no exit status", r.gotExit, r.err)
			}
		case <-time.After(testTimeout):
			t.Fatal("the exchange did not end when the agent stopped")
		}
		f.host.Fake.SetExec(nil)
		f.host.StartAgent()
		eventually(t, "the restarted agent to serve", func() bool {
			r := exchange(ctx, client, f.start("after-restart"), "", nil)
			return r.err == nil && r.gotExit && r.exitCode == 0
		})
	})

	t.Run("the connection to the agent is cut", func(t *testing.T) {
		cancelled := make(chan struct{}, 1)
		f.host.Fake.SetExec(blockUntilCancelled(cancelled))
		ctx, cancel := context.WithTimeout(context.Background(), 2*testTimeout)
		defer cancel()
		proxy := newCutProxy(t, f.host.Address)
		client := execv1.NewMicroVMExecClient(conn(t, proxy.addr(), f.host.ServingCAFile, f.holder.Token))
		started, done := runLong(ctx, client, f.start("cut-connection"))
		select {
		case <-started:
		case <-time.After(testTimeout):
			t.Fatal("the command never started")
		}
		proxy.cut()
		select {
		case r := <-done:
			if r.err == nil || r.gotExit {
				t.Errorf("exec across a cut connection = (sent %v, %v), want a failure and no exit status", r.gotExit, r.err)
			}
		case <-time.After(testTimeout):
			t.Fatal("the exchange did not end when its connection was cut")
		}
		select {
		case <-cancelled:
		case <-time.After(testTimeout):
			t.Fatal("flintlockd's stream was not cancelled when the caller's connection went")
		}
	})
}

//= docs/requirements/05-exec-agent.md#relay
//= type=test
//# If `flintlockd` has not answered for the requested MicroVM and
//# accepted the request's exec stream within the configured deadline, then
//# the Exec Agent SHALL end the response as a stream failure.

// TestAHungFlintlockdFailsWithinTheDeadline has flintlockd stop answering
// without closing its connections. An exec through the agent, from a
// caller whose own context allows a minute, ends as a stream failure
// within the agent's open deadline of a second, and nothing reaches
// flintlockd.
func TestAHungFlintlockdFailsWithinTheDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, hostOptions{ExecOpenTimeout: time.Second})
	f.bind("claim")
	f.host.Fake.SetFaults(fakeflintlock.Faults{Unresponsive: true})
	defer f.host.Fake.SetFaults(fakeflintlock.Faults{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	begun := time.Now()
	r := exchange(ctx, f.rawExec(f.holder.Token), f.start("hung"), "", nil)
	if status.Code(r.err) != codes.Unavailable || r.gotExit {
		t.Errorf("exec to a hung flintlockd = (sent %v, %v), want unavailable and no exit status", r.gotExit, r.err)
	}
	if elapsed := time.Since(begun); elapsed > 5*time.Second {
		t.Errorf("the exchange took %s to fail, want it within the one-second open deadline", elapsed)
	}
	f.host.Fake.SetFaults(fakeflintlock.Faults{})
	if f.ran("hung") {
		t.Error("a command reached flintlockd although it never opened its stream")
	}
}
