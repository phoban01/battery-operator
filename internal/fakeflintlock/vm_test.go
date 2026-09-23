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
	"testing"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/phoban01/battery-operator/internal/clock"
)

// ownUID is a uid a test chooses for a MicroVM.
const ownUID = "my-uid"

// waitBooted waits for uid's boot to finish after the clock was advanced.
func waitBooted(t *testing.T, s *Server, uid string) {
	t.Helper()
	ch := s.bootDone(uid)
	if ch == nil {
		t.Fatalf("bootDone(%s): unknown uid", uid)
	}
	select {
	case <-ch:
	case <-time.After(testTimeout):
		t.Fatalf("microvm %s did not finish booting", uid)
	}
}

// TestBootDelay drives the boot with a fake clock: PENDING with no
// vsock_path until BootDelay has elapsed, then CREATED with vsock_path.
func TestBootDelay(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(testEpoch)
	s := newTestServer(t, Config{BootDelay: 2 * time.Second, Clock: clk})
	conn := memConn(t, s)
	vms := mvmv1.NewMicroVMClient(conn)
	ctx := testCtx(t)

	vm := createVM(t, conn, nil)
	uid := vm.GetSpec().GetUid()
	if vm.GetStatus().GetState() != types.MicroVMStatus_PENDING || vm.GetStatus().GetVsockPath() != "" {
		t.Fatalf("right after create: %v, want PENDING without vsock_path", vm.GetStatus())
	}

	clk.Advance(time.Second)
	got, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetMicrovm().GetStatus().GetState() != types.MicroVMStatus_PENDING {
		t.Errorf("after 1s of a 2s boot: state = %s, want PENDING", got.GetMicrovm().GetStatus().GetState())
	}

	clk.Advance(time.Second)
	waitBooted(t, s, uid)
	got, err = vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	booted := got.GetMicrovm()
	if booted.GetStatus().GetState() != types.MicroVMStatus_CREATED || booted.GetStatus().GetVsockPath() == "" {
		t.Errorf("after the boot delay: %v, want CREATED with vsock_path", booted.GetStatus())
	}
	if booted.GetVersion() <= vm.GetVersion() {
		t.Errorf("version did not advance on boot: %d -> %d", vm.GetVersion(), booted.GetVersion())
	}
	if !booted.GetSpec().GetUpdatedAt().AsTime().After(vm.GetSpec().GetUpdatedAt().AsTime()) {
		t.Error("updated_at did not advance on boot")
	}
}

// TestCreateDefaultsAndDuplicates covers uid assignment, id and namespace
// defaults, honouring a supplied uid, and rejecting a duplicate.
func TestCreateDefaultsAndDuplicates(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	conn := memConn(t, s)
	vms := mvmv1.NewMicroVMClient(conn)
	ctx := testCtx(t)

	vm := createVM(t, conn, &types.MicroVMSpec{})
	if vm.GetSpec().GetUid() == "" || vm.GetSpec().GetId() != vm.GetSpec().GetUid() || vm.GetSpec().GetNamespace() != "default" {
		t.Errorf("defaults: %v", vm.GetSpec())
	}
	if vm.GetSpec().GetCreatedAt() == nil {
		t.Error("created_at not set")
	}

	own := createVM(t, conn, &types.MicroVMSpec{Uid: proto.String(ownUID), Id: "x", Namespace: "ns"})
	if own.GetSpec().GetUid() != ownUID {
		t.Errorf("supplied uid not honoured: %q", own.GetSpec().GetUid())
	}
	_, err := vms.CreateMicroVM(ctx, &mvmv1.CreateMicroVMRequest{Microvm: &types.MicroVMSpec{Uid: proto.String(ownUID)}})
	if status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate uid: code = %s, want AlreadyExists", status.Code(err))
	}

	all, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{})
	if err != nil || len(all.GetMicrovm()) != 2 {
		t.Errorf("ListMicroVMs(all) = %d, %v; want 2", len(all.GetMicrovm()), err)
	}
	name := "x"
	byName, err := vms.ListMicroVMs(ctx, &mvmv1.ListMicroVMsRequest{Name: &name})
	if err != nil || len(byName.GetMicrovm()) != 1 || byName.GetMicrovm()[0].GetSpec().GetUid() != ownUID {
		t.Errorf("ListMicroVMs(name=x) = %v, %v; want my-uid only", byName.GetMicrovm(), err)
	}

	// The MicroVM returned is a copy: changing it does not change the
	// Server's record.
	own.Status.State = types.MicroVMStatus_FAILED
	again, _ := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: ownUID})
	if again.GetMicrovm().GetStatus().GetState() != types.MicroVMStatus_CREATED {
		t.Error("CreateMicroVM returned the Server's own record rather than a copy")
	}
}

// TestCreateFailsFault: with CreateFails set the boot ends in FAILED with
// no vsock_path, and the MicroVM cannot run commands.
func TestCreateFailsFault(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(testEpoch)
	s := newTestServer(t, Config{BootDelay: time.Second, Clock: clk})
	conn := memConn(t, s)

	uid := createVM(t, conn, nil).GetSpec().GetUid()
	s.SetFaults(Faults{CreateFails: true})
	clk.Advance(time.Second)
	waitBooted(t, s, uid)

	got, err := mvmv1.NewMicroVMClient(conn).GetMicroVM(testCtx(t), &mvmv1.GetMicroVMRequest{Uid: uid})
	if err != nil {
		t.Fatalf("GetMicroVM: %v", err)
	}
	if got.GetMicrovm().GetStatus().GetState() != types.MicroVMStatus_FAILED || got.GetMicrovm().GetStatus().GetVsockPath() != "" {
		t.Errorf("status = %v, want FAILED without vsock_path", got.GetMicrovm().GetStatus())
	}
	if res := runExec(t, conn, command(uid, "true")); status.Code(res.err) != codes.FailedPrecondition {
		t.Errorf("exec on a FAILED microvm: err=%v exit=%v; want FailedPrecondition", res.err, res.exit)
	}

	// Zero boot delay reads the fault at create time.
	s2 := newTestServer(t, Config{})
	s2.SetFaults(Faults{CreateFails: true})
	if vm, err := s2.CreateMicroVM(&types.MicroVMSpec{}); err != nil || vm.GetStatus().GetState() != types.MicroVMStatus_FAILED {
		t.Errorf("immediate boot with CreateFails: %v, %v; want FAILED", vm.GetStatus(), err)
	}
}

// TestDeleteDuringBoot: deleting a PENDING MicroVM stops its boot without
// bringing the record back.
func TestDeleteDuringBoot(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(testEpoch)
	s := newTestServer(t, Config{BootDelay: time.Second, Clock: clk})
	conn := memConn(t, s)
	vms := mvmv1.NewMicroVMClient(conn)

	uid := createVM(t, conn, nil).GetSpec().GetUid()
	booted := s.bootDone(uid)
	if _, err := vms.DeleteMicroVM(testCtx(t), &mvmv1.DeleteMicroVMRequest{Uid: uid}); err != nil {
		t.Fatalf("DeleteMicroVM: %v", err)
	}
	clk.Advance(time.Second)
	// Close waits for the boot goroutine to see the fired timer and give up.
	_ = s.Close()
	select {
	case <-booted:
		t.Error("boot completed for a deleted microvm")
	default:
	}
	s.mu.Lock()
	n := len(s.vms)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("%d microvms after the delete, want none", n)
	}
}

// TestGetAndDeleteErrors covers the status codes of lookups by uid.
func TestGetAndDeleteErrors(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, Config{})
	vms := mvmv1.NewMicroVMClient(memConn(t, s))
	ctx := testCtx(t)
	if _, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{Uid: missingUID}); status.Code(err) != codes.NotFound {
		t.Errorf("GetMicroVM(missing) = %v, want NotFound", err)
	}
	if _, err := vms.GetMicroVM(ctx, &mvmv1.GetMicroVMRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetMicroVM(empty) = %v, want InvalidArgument", err)
	}
	if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{Uid: missingUID}); status.Code(err) != codes.NotFound {
		t.Errorf("DeleteMicroVM(missing) = %v, want NotFound", err)
	}
	if _, err := vms.DeleteMicroVM(ctx, &mvmv1.DeleteMicroVMRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteMicroVM(empty) = %v, want InvalidArgument", err)
	}
}
