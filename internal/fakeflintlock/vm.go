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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/phoban01/battery-operator/internal/clock"
)

// microVM is one MicroVM record. Its fields are guarded by Server.mu.
type microVM struct {
	vm *types.MicroVM
	// booted is closed when the boot has finished and the state moved off
	// PENDING, for tests that wait on it.
	booted chan struct{}
	// execs cancel the commands running in this MicroVM, so that
	// DeleteMicroVM can end them.
	execs  map[uint64]context.CancelCauseFunc
	nextID uint64
}

// The store's errors are gRPC status errors, with the codes a caller of
// flintlockd has to handle, so that the services return them as they are.

func errNotFound(uid string) error {
	return status.Errorf(codes.NotFound, "microvm spec %s not found", uid)
}

func errClosedStatus() error {
	return status.Error(codes.Unavailable, errClosed.Error())
}

// createMicroVM records a new MicroVM in state PENDING and starts its boot.
// A spec without a uid gets one; a uid already known is ALREADY_EXISTS. An
// empty id becomes the uid and an empty namespace "default", which is more
// forgiving than flintlockd and is all a template from battery needs.
func (s *Server) createMicroVM(spec *types.MicroVMSpec) (*types.MicroVM, error) {
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid create microvm request: MicroVMSpec required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosedStatus()
	}

	spec = proto.CloneOf(spec)
	uid := spec.GetUid()
	if uid == "" {
		uid = newUID()
		spec.Uid = proto.String(uid)
	}
	if _, exists := s.vms[uid]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "microvm %s already exists", uid)
	}
	if spec.GetId() == "" {
		spec.Id = uid
	}
	if spec.GetNamespace() == "" {
		spec.Namespace = "default"
	}
	now := timestamppb.New(s.clk.Now())
	spec.CreatedAt = now
	spec.UpdatedAt = now

	rec := &microVM{
		vm: &types.MicroVM{
			Version: 1,
			Spec:    spec,
			Status:  &types.MicroVMStatus{State: types.MicroVMStatus_PENDING},
		},
		booted: make(chan struct{}),
		execs:  make(map[uint64]context.CancelCauseFunc),
	}
	s.vms[uid] = rec

	if s.cfg.BootDelay <= 0 {
		s.finishBootLocked(rec)
	} else {
		timer := s.clk.NewTimer(s.cfg.BootDelay)
		s.wg.Add(1)
		go s.bootMicroVM(rec, timer)
	}
	return proto.CloneOf(rec.vm), nil
}

// bootMicroVM waits out the boot delay and completes the boot. It gives up
// when the Server is closed first, leaving the MicroVM PENDING, or when the
// MicroVM was deleted while booting.
func (s *Server) bootMicroVM(rec *microVM, timer clock.Timer) {
	defer s.wg.Done()
	select {
	case <-timer.C():
	case <-s.ctx.Done():
		timer.Stop()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	uid := rec.vm.GetSpec().GetUid()
	if s.vms[uid] != rec || rec.vm.GetStatus().GetState() != types.MicroVMStatus_PENDING {
		return
	}
	s.finishBootLocked(rec)
}

// finishBootLocked moves a PENDING MicroVM to CREATED, with vsock_path and
// an interface status for each interface of its spec, or to FAILED when
// the CreateFails fault is set. Callers hold s.mu.
func (s *Server) finishBootLocked(rec *microVM) {
	if s.Faults().CreateFails {
		rec.vm.Status.State = types.MicroVMStatus_FAILED
	} else {
		uid := rec.vm.GetSpec().GetUid()
		rec.vm.Status.State = types.MicroVMStatus_CREATED
		rec.vm.Status.VsockPath = path.Join("/fakeflintlock", s.cfg.Name, uid, "vsock.sock")
		rec.vm.Status.NetworkInterfaces = interfaceStatus(uid, rec.vm.GetSpec().GetInterfaces())
	}
	rec.vm.Spec.UpdatedAt = timestamppb.New(s.clk.Now())
	rec.vm.Version++
	close(rec.booted)
}

// interfaceStatus makes up a host device for each interface, keyed by
// device id as flintlockd keys it.
func interfaceStatus(uid string, ifaces []*types.NetworkInterface) map[string]*types.NetworkInterfaceStatus {
	if len(ifaces) == 0 {
		return nil
	}
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	out := make(map[string]*types.NetworkInterfaceStatus, len(ifaces))
	for i, iface := range ifaces {
		mac := iface.GetGuestMac()
		if mac == "" {
			mac = fmt.Sprintf("02:00:00:00:00:%02x", i+1)
		}
		out[iface.GetDeviceId()] = &types.NetworkInterfaceStatus{
			HostDeviceName: fmt.Sprintf("fl%s%d", short, i),
			Index:          int32(i + 1),
			MacAddress:     mac,
		}
	}
	return out
}

// bootDone returns the channel closed when uid's boot has finished, or nil
// when uid is unknown.
func (s *Server) bootDone(uid string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.vms[uid]
	if !ok {
		return nil
	}
	return rec.booted
}

// getMicroVM returns a copy of the named MicroVM or NOT_FOUND.
func (s *Server) getMicroVM(uid string) (*types.MicroVM, error) {
	if uid == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosedStatus()
	}
	rec, ok := s.vms[uid]
	if !ok {
		return nil, errNotFound(uid)
	}
	return proto.CloneOf(rec.vm), nil
}

// listMicroVMs returns copies of the MicroVMs in namespace, or in every
// namespace when it is empty, narrowed to one id when name is set, sorted
// by uid.
func (s *Server) listMicroVMs(namespace string, name *string) ([]*types.MicroVM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosedStatus()
	}
	out := make([]*types.MicroVM, 0, len(s.vms))
	for _, rec := range s.vms {
		spec := rec.vm.GetSpec()
		if namespace != "" && spec.GetNamespace() != namespace {
			continue
		}
		if name != nil && spec.GetId() != *name {
			continue
		}
		out = append(out, proto.CloneOf(rec.vm))
	}
	slices.SortFunc(out, func(a, b *types.MicroVM) int {
		return strings.Compare(a.GetSpec().GetUid(), b.GetSpec().GetUid())
	})
	return out, nil
}

// deleteMicroVM ends the MicroVM's running commands and forgets it. An
// unknown uid is NOT_FOUND. flintlockd moves the MicroVM through DELETING
// asynchronously; the fake deletes at once, so a GetMicroVM right after
// is NOT_FOUND.
func (s *Server) deleteMicroVM(uid string) error {
	if uid == "" {
		return status.Error(codes.InvalidArgument, "invalid request")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosedStatus()
	}
	rec, ok := s.vms[uid]
	if !ok {
		return errNotFound(uid)
	}
	rec.vm.Status.State = types.MicroVMStatus_DELETING
	delete(s.vms, uid)
	for _, cancel := range rec.execs {
		cancel(errVMDeleted)
	}
	return nil
}

// attachExec checks that uid can run a command, as flintlockd does before
// it dials the guest agent, and registers cancel so that DeleteMicroVM can
// end the command. It returns the function that unregisters it, which the
// caller runs when the command is over: Close waits for that.
func (s *Server) attachExec(uid string, cancel context.CancelCauseFunc) (detach func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosedStatus()
	}
	rec, ok := s.vms[uid]
	if !ok {
		return nil, errNotFound(uid)
	}
	if state := rec.vm.GetStatus().GetState(); state != types.MicroVMStatus_CREATED {
		return nil, status.Errorf(codes.FailedPrecondition, "microvm is not ready (state: %s)", strings.ToLower(state.String()))
	}
	id := rec.nextID
	rec.nextID++
	rec.execs[id] = cancel
	s.wg.Add(1)
	return func() {
		s.mu.Lock()
		delete(rec.execs, id)
		s.mu.Unlock()
		s.wg.Done()
	}, nil
}

// newUID returns a fresh MicroVM uid. flintlockd uses ULIDs; any unique
// token does for the fake.
func newUID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	return hex.EncodeToString(b[:])
}
