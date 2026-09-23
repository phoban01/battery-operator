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

package fakebattery

import (
	"context"
	"errors"
	"fmt"
	"io"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
)

// runHooks runs a Pool's create or pre-lease commands in vm over the Host's
// MicroVMExec service, the way battery does, and fails on the first command
// that errors or exits non-zero. An injected hook failure fires at this
// point whether or not the Pool declares any command, so tests can exercise
// VM_HOOK_FAILED and the failure policy without a guest agent. Before
// create hooks it waits for the guest agent to answer, because a MicroVM
// that has just reached CREATED has usually not started it yet.
func (b *Battery) runHooks(ctx context.Context, vm *vmState, kind HookKind, cmds []string) error {
	if b.consumeHookFailure(vm.pool, kind) {
		return fmt.Errorf("fault injection: %s hook failed", kind)
	}
	if len(cmds) == 0 {
		return nil
	}
	h, err := b.cfg.Hosts.get(vm.host)
	if err != nil {
		return err
	}
	if kind == HookCreate {
		if err := b.waitGuestReady(ctx, h, vm.uid); err != nil {
			return err
		}
	}
	for _, cmd := range cmds {
		code, err := execHook(ctx, h, vm.uid, cmd)
		if err != nil {
			return fmt.Errorf("%s hook %q: %w", kind, cmd, err)
		}
		if code != 0 {
			return fmt.Errorf("%s hook %q: exit code %d", kind, cmd, code)
		}
	}
	return nil
}

// consumeHookFailure takes one pending HookFailure for the Pool and hook
// kind, if any. A HookFailure with a zero Pool matches every Pool.
func (b *Battery) consumeHookFailure(key poolKey, kind HookKind) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.faults.HookFailures {
		hf := &b.faults.HookFailures[i]
		if hf.Hook != kind || hf.Remaining <= 0 {
			continue
		}
		if hf.Pool != (PoolRef{}) && keyOf(hf.Pool) != key {
			continue
		}
		hf.Remaining--
		if hf.Remaining == 0 {
			b.faults.HookFailures = append(b.faults.HookFailures[:i], b.faults.HookFailures[i+1:]...)
		}
		return true
	}
	return false
}

// waitGuestReady retries a no-op command until the guest agent answers or
// Config.ReadyTimeout passes on the fake's clock. Like battery's WaitReady
// it treats any exec failure as "not yet".
func (b *Battery) waitGuestReady(ctx context.Context, h host, uid string) error {
	deadline := b.cfg.Clock.Now().Add(b.cfg.ReadyTimeout)
	for {
		_, err := execHook(ctx, h, uid, "true")
		if err == nil {
			return nil
		}
		if isCtxErr(err) {
			return err
		}
		if !b.cfg.Clock.Now().Before(deadline) {
			return fmt.Errorf("guest agent of %s not ready within %s: %w", uid, b.cfg.ReadyTimeout, err)
		}
		if err := b.sleep(ctx, createPollInterval); err != nil {
			return err
		}
	}
}

// execHook runs one shell command in a MicroVM through ExecCommand and
// returns its exit code. It sends ExecStart with shell set and no stdin,
// half-closes, and reads until exit_code; an error message from the server
// or a stream that ends first is an error.
func execHook(ctx context.Context, h host, uid, cmd string) (int32, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := h.exec.ExecCommand(ctx)
	if err != nil {
		return 0, fmt.Errorf("open exec stream: %w", err)
	}
	start := &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{
		Uid:   uid,
		Cmd:   cmd,
		Shell: true,
	}}}
	if err := stream.Send(start); err != nil {
		return 0, fmt.Errorf("send exec start: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return 0, fmt.Errorf("close send: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return 0, errors.New("exec stream closed before exit code")
		}
		if err != nil {
			return 0, fmt.Errorf("exec stream: %w", err)
		}
		switch payload := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_Error:
			return 0, fmt.Errorf("exec error: %s", payload.Error)
		case *execv1.ExecCommandResponse_ExitCode:
			return payload.ExitCode, nil
		case *execv1.ExecCommandResponse_Stdout, *execv1.ExecCommandResponse_Stderr:
		}
	}
}
