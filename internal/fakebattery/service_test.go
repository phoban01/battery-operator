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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL serve battery v0.3.3's `PoolAdmin`,
//# `Lease` and `Events` services over gRPC with the generated server stubs.

// TestServesTheThreeServicesOverGRPC drives all three services through the
// generated clients over a real TCP gRPC connection to a served fake, so
// that the test fails if any service stops being registered or a handler
// stops answering. It is the wire-level counterpart of the in-memory
// connection the rest of the tests use.
func TestServesTheThreeServicesOverGRPC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	host := newTestHost(t, hostA)
	hosts := NewHosts()
	hosts.Add(host.name, "host-a:9090", host.conn)
	b := New(Config{
		Listen:            defaultListen,
		Hosts:             hosts,
		Clock:             clock.NewFake(testEpoch),
		ReconcileInterval: testInterval,
	})

	served := make(chan error, 1)
	go func() { served <- b.Serve(ctx) }()
	select {
	case <-b.Ready():
	case err := <-served:
		t.Fatalf("Serve returned before listening: %v", err)
	}

	conn, err := grpc.NewClient(b.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", b.Addr(), err)
	}
	defer func() { _ = conn.Close() }()
	admin := poolmgrv1.NewPoolAdminClient(conn)
	lease := poolmgrv1.NewLeaseClient(conn)
	events := poolmgrv1.NewEventsClient(conn)

	// Events first, so that the whole life of the Pool is on the stream.
	stream, err := events.Subscribe(ctx, &poolmgrv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Events.Subscribe: %v", err)
	}

	spec := &poolmgrv1.PoolSpec{
		Name:                     "wire",
		Namespace:                testNamespace,
		Size:                     1,
		FlintlockHosts:           []string{host.name},
		MicrovmTemplate:          &types.MicroVMSpec{Namespace: testNamespace, Vcpu: 1, MemoryInMb: 256},
		ReplenishmentStrategy:    &poolmgrv1.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatExpiryThreshold: durationpb.New(30 * time.Second),
	}
	ref := &poolmgrv1.PoolRef{Name: spec.GetName(), Namespace: spec.GetNamespace()}

	// PoolAdmin.
	if _, err := admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: spec}); err != nil {
		t.Fatalf("PoolAdmin.CreatePool: %v", err)
	}
	if _, err := admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: spec}); statusCode(t, err) != codes.AlreadyExists {
		t.Fatalf("PoolAdmin.CreatePool twice: code %v, want ALREADY_EXISTS", statusCode(t, err))
	}
	// Events: the Pool fills without the clock moving, because a create
	// kicks the control loop.
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_AVAILABLE)

	got, err := admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: ref})
	if err != nil {
		t.Fatalf("PoolAdmin.GetPool: %v", err)
	}
	if n := got.GetStatus().GetAvailableCount(); n != 1 {
		t.Fatalf("GetPool available_count = %d, want 1", n)
	}
	list, err := admin.ListPools(ctx, &poolmgrv1.ListPoolsRequest{})
	if err != nil {
		t.Fatalf("PoolAdmin.ListPools: %v", err)
	}
	if len(list.GetPools()) != 1 || list.GetPools()[0].GetSpec().GetName() != "wire" {
		t.Fatalf("ListPools = %v, want the one wire pool", list.GetPools())
	}
	spec.Size = 2
	if _, err := admin.UpdatePool(ctx, &poolmgrv1.UpdatePoolRequest{Spec: spec}); err != nil {
		t.Fatalf("PoolAdmin.UpdatePool: %v", err)
	}
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_AVAILABLE)

	// Lease.
	claim, err := lease.ClaimVM(ctx, &poolmgrv1.ClaimVMRequest{Pool: ref})
	if err != nil {
		t.Fatalf("Lease.ClaimVM: %v", err)
	}
	if claim.GetLeaseId() == "" || claim.GetVmUid() == "" {
		t.Fatalf("ClaimVMResponse = %v, want a lease id and a vm uid", claim)
	}
	hb, err := lease.Heartbeat(ctx, &poolmgrv1.HeartbeatRequest{LeaseId: claim.GetLeaseId()})
	if err != nil {
		t.Fatalf("Lease.Heartbeat: %v", err)
	}
	if want := testEpoch.Add(30 * time.Second); !hb.GetExpiresAt().AsTime().Equal(want) {
		t.Fatalf("Heartbeat expires_at = %s, want %s", hb.GetExpiresAt().AsTime(), want)
	}
	if _, err := lease.ReleaseVM(ctx, &poolmgrv1.ReleaseVMRequest{LeaseId: claim.GetLeaseId()}); err != nil {
		t.Fatalf("Lease.ReleaseVM: %v", err)
	}
	waitProtoEvent(t, stream, poolmgrv1.EventType_VM_DELETED_ON_RELEASE)

	// battery refuses to delete a Pool that owns MicroVMs; one that never
	// had any goes at once.
	if _, err := admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: ref}); statusCode(t, err) != codes.FailedPrecondition {
		t.Fatalf("PoolAdmin.DeletePool of a filled pool: code %v, want FAILED_PRECONDITION", statusCode(t, err))
	}
	spec.Name, spec.Size = "empty", 0
	ref = &poolmgrv1.PoolRef{Name: spec.GetName(), Namespace: spec.GetNamespace()}
	if _, err := admin.CreatePool(ctx, &poolmgrv1.CreatePoolRequest{Spec: spec}); err != nil {
		t.Fatalf("PoolAdmin.CreatePool: %v", err)
	}
	if _, err := admin.DeletePool(ctx, &poolmgrv1.DeletePoolRequest{Ref: ref}); err != nil {
		t.Fatalf("PoolAdmin.DeletePool: %v", err)
	}
	if _, err := admin.GetPool(ctx, &poolmgrv1.GetPoolRequest{Ref: ref}); statusCode(t, err) != codes.NotFound {
		t.Fatalf("GetPool after DeletePool: code %v, want NOT_FOUND", statusCode(t, err))
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvms left on the host after shutdown, want 0", n)
	}
}

// waitProtoEvent reads the generated Event stream until an event of type typ
// arrives.
func waitProtoEvent(t *testing.T, stream grpc.ServerStreamingClient[poolmgrv1.Event], typ poolmgrv1.EventType) *poolmgrv1.Event {
	t.Helper()
	for {
		e, err := stream.Recv()
		if err != nil {
			t.Fatalf("waiting for %s on the Events stream: %v", typ, err)
		}
		if e.GetType() == typ {
			return e
		}
	}
}

//= docs/requirements/08-test-doubles.md#fake-battery
//= type=test
//# The fake battery SHALL create, place and delete MicroVMs only
//# through the fake `flintlockd`.

// TestMicroVMsGoThroughTheFakeFlintlockd proves that every MicroVM the fake
// owns was created on a fake flintlockd and went back to it on deletion,
// that its hooks ran there, that a Host the fake does not know is never
// created on, and that a fake flintlockd reached over TCP with mutual TLS,
// as battery reaches flintlockd, behaves the same as one in memory.
func TestMicroVMsGoThroughTheFakeFlintlockd(t *testing.T) {
	t.Run("created and deleted on the fake flintlockd", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA, hostB)
		h.createPool(h.spec("pool", 2, hostA, hostB))

		// Every MicroVM the fake reports is one a fake flintlockd created
		// and still holds; the fake has no way to make one up.
		uids := h.uids()
		if len(uids) != 2 {
			t.Fatalf("VMs() = %v, want two microvms", uids)
		}
		for _, uid := range uids {
			if !h.stubs[hostA].has(uid) && !h.stubs[hostB].has(uid) {
				t.Fatalf("microvm %q is on neither fake flintlockd", uid)
			}
		}
		for _, name := range []string{hostA, hostB} {
			if created, _ := h.stubs[name].counts(); created != 1 {
				t.Fatalf("%s created %d microvms, want 1", name, created)
			}
		}

		// The deletion goes the same way.
		claim := h.claim("pool")
		h.release(claim.LeaseID)
		var deleted []string
		for _, s := range h.stubs {
			s.mu.Lock()
			deleted = append(deleted, s.deleted...)
			s.mu.Unlock()
		}
		if len(deleted) != 1 || deleted[0] != claim.VMUID {
			t.Fatalf("deleted on the Hosts = %v, want [%s]", deleted, claim.VMUID)
		}
	})

	t.Run("pool hooks run in the microvm through MicroVMExec", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		spec := h.spec("pool", 1, hostA)
		spec.CreateCommands = []string{setupHook}
		spec.PreLeaseCommands = []string{"/opt/pre-lease.sh"}
		// The claim must not start a replacement, whose own create hooks
		// would run while the assertion below reads the Host's log.
		spec.Replenishment = replenishment{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(1),
		}
		h.createPool(spec)
		claim := h.claim("pool")

		// The guest-agent readiness probe comes first, then the create
		// commands, then the pre-lease commands of the claim.
		want := []string{"true", setupHook, "/opt/pre-lease.sh"}
		if got := h.stubs[hostA].commands(); !slices.Equal(got, want) {
			t.Fatalf("commands run on the host = %v, want %v", got, want)
		}
		h.release(claim.LeaseID)
	})

	t.Run("a hook that exits non-zero fails the microvm", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		host.fl.SetExec(func(_ context.Context, e *fakeflintlock.Exec) (int32, error) {
			if e.Start.GetCmd() == setupHook {
				return 3, nil
			}
			return 0, nil
		})
		spec := h.spec("pool", 1, hostA)
		spec.CreateCommands = []string{setupHook}
		if _, err := h.client.CreatePool(h.ctx, spec); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
		failed := h.waitEvent(poolmgrv1.EventType_VM_HOOK_FAILED)
		if p := payloadOf(t, failed); p["hook"] != string(HookCreate) {
			t.Fatalf("VM_HOOK_FAILED payload = %v, want the create hook", p)
		}
		h.waitDeletion(host, failed.VMUID)
		assertDeleted(t, host, failed.VMUID)
	})

	t.Run("a deletion the host refuses is retried", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		host := h.stubs[hostA]
		spec := h.spec("pool", 1, hostA)
		spec.Replenishment = replenishment{
			Type:    poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			MinSize: minSize(1),
		}
		h.createPool(spec)
		claim := h.claim("pool")

		host.set(func(s *testHost) { s.deleteErr = errors.New("host busy") })
		err := h.client.ReleaseVM(h.ctx, claim.LeaseID)
		if statusCode(t, err) != codes.Unavailable {
			t.Fatalf("ReleaseVM while the host refuses = %v, want UNAVAILABLE", err)
		}
		if _, deleted := host.counts(); deleted != 0 {
			t.Fatalf("%d microvms deleted, want none: the host refused", deleted)
		}
		if live := host.live(); live != 1 {
			t.Fatalf("%d microvms on the host, want the one the host would not delete", live)
		}

		// The control loop retries until the Host takes it.
		host.set(func(s *testHost) { s.deleteErr = nil })
		h.advance(testInterval)
		deleted := h.waitEvent(poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
		if deleted.VMUID != claim.VMUID {
			t.Fatalf("VM_DELETED_ON_RELEASE for %q, want the released microvm %q", deleted.VMUID, claim.VMUID)
		}
		assertDeleted(t, host, claim.VMUID)
		if leases := h.b.Leases(); len(leases) != 0 {
			t.Fatalf("leases after the retried deletion = %+v, want none", leases)
		}
	})

	t.Run("a host the fake does not know is never created on", func(t *testing.T) {
		h := newHarness(t, Config{}, hostA)
		if _, err := h.client.CreatePool(h.ctx, h.spec("pool", 2, hostA, "gone")); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		h.waitEvent(poolmgrv1.EventType_VM_AVAILABLE)
		if got := h.vmHosts(); got["gone"] != 0 || got[hostA] != 2 {
			t.Fatalf("placement = %v, want both microvms on host-a and none on the unknown host", got)
		}
	})

	t.Run("a fake flintlockd over mutual TLS behaves the same", func(t *testing.T) {
		certs, err := fakeflintlock.WriteTestCerts(t.TempDir())
		if err != nil {
			t.Fatalf("WriteTestCerts: %v", err)
		}
		hosts := NewHosts()
		servers := map[string]*fakeflintlock.Server{}
		for _, name := range []string{hostA, hostB} {
			fl := fakeflintlock.New(fakeflintlock.Config{Name: name, TLS: certs.ServerTLS(true)})
			servers[name] = fl
			addr := serveFlintlockd(t, fl)
			conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientTLS(t, certs)))
			if err != nil {
				t.Fatalf("dial %s: %v", addr, err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			hosts.Add(name, addr, conn)
		}
		h := newHarness(t, Config{Hosts: hosts})
		h.createPool(h.spec("pool", 2, hostA, hostB))
		if got := h.vmHosts(); got[hostA] != 1 || got[hostB] != 1 {
			t.Fatalf("placement over TLS hosts = %v, want one each", got)
		}
		for name, fl := range servers {
			if n := len(fl.MicroVMs()); n != 1 {
				t.Fatalf("%s holds %d microvms, want 1", name, n)
			}
		}
		claim := h.claim("pool")
		if claim.Address != servers[claim.Host].Addr() {
			t.Fatalf("claim reports %s at %q, want its listen address %q", claim.Host, claim.Address, servers[claim.Host].Addr())
		}
	})
}

// serveFlintlockd serves fl over TCP until the test ends and returns its
// address.
func serveFlintlockd(t *testing.T, fl *fakeflintlock.Server) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- fl.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("fake flintlockd Serve returned %v", err)
		}
	})
	select {
	case <-fl.Ready():
	case err := <-served:
		t.Fatalf("fake flintlockd did not start: %v", err)
	}
	return fl.Addr()
}

// clientTLS is battery's side of mutual TLS with the test certificates.
func clientTLS(t *testing.T, certs *fakeflintlock.TestCerts) credentials.TransportCredentials {
	t.Helper()
	ca, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("CA file has no certificate")
	}
	cert, err := tls.LoadX509KeyPair(certs.ClientCertFile, certs.ClientKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
}
