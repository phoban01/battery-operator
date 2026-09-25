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

package controller

// The Claim Controller against its Quint model (#62): each trace of
// specs/quint/claims.qnt in testdata/claims-traces is replayed step by
// step against the controller's subreconciler chain, with
// controller-runtime's fake client, a scripted battery and a fake clock,
// and after every step the claims, battery's Leases and what the controller
// holds in memory are compared with the trace's state. No API server:
// ADR 0006.
//
// How each of the model's steps maps to Go is in replayStep. The short
// version:
//
//   - The environment's steps (createClaim, deleteClaim, renewClaim) are
//     writes through the fake client; tick advances the fake clock.
//   - battery's steps (replenish, batteryExpire, batteryStop, batteryStart,
//     dropEvent) change the scripted battery, and batteryStart, like
//     crashController, sets the controller's recovery due.
//   - Each ctl* step for a claim is one reconcile of it: the scope, the
//     controller's chain, and the patch. The controller's steps that
//     answer (ctlClaimVM, ctlHeartbeat, ctlReleaseVM) run the chain and
//     keep the scope unpatched, as what the controller holds in memory; the
//     model's next step for the claim (ctlWriteBound, ctlWriteRenewed,
//     ctlRemoveFinalizer) patches it. That is the hook between the call to
//     battery and the patch that #62 asks for, and a crash
//     (crashController) between the two throws the scope away.
//   - ctlEventDeleted is the event watcher's handling of one deletion
//     (claimEvents.handle) and the reconciles it sends; ctlRecover is one
//     recovery (claimRecovery.run) and the reconciles it sends.
//
// Where the model and the controller are known to differ, knownDivergences
// names the difference and its issue, and the replay of that trace stops at
// the step that reaches it. Any other difference fails the test.
//
// The traces are written by `make claims-traces` (hack/claims-traces.sh)
// from specs/quint/claims_replay.qnt with a fixed seed, and committed, so
// that `make test` needs no quint and a failure replays the same way every
// time. `make quint` checks that they are up to date with the model.
// BATTERY_OPERATOR_CLAIMS_TRACES, a glob, replays other traces instead.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/go-logr/logr"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/controller/claim"
	"github.com/phoban01/battery-operator/internal/controller/claimscope"
)

// modelClaims is claims.qnt's CLAIMS.
var modelClaims = []string{"a", "b", "c"}

// The names of claims.qnt's steps and tags that the replay looks at more
// than once.
const (
	stepCreateClaim    = "createClaim"
	stepEventDeleted   = "ctlEventDeleted"
	stepRecover        = "ctlRecover"
	stepExpiryPassed   = "ctlExpiryPassed"
	stepListRenewed    = "ctlListRenewed"
	stepListUnanswered = "ctlListUnanswered"

	slotLive = "Live"
)

// The in-flight answers of claims.qnt's InFlight, which the replay keeps
// as an unpatched scope.
const (
	idle              = "Idle"
	claimAnswered     = "ClaimAnswered"
	heartbeatAnswered = "HeartbeatAnswered"
	releaseAnswered   = "ReleaseAnswered"
)

// ctlStep is how one of the model's controller steps for a claim is
// replayed: how battery answers the calls, which calls the chain must make,
// and what the controller holds afterwards, if anything.
type ctlStep struct {
	mode  callMode
	calls []string
	// hold is the in-flight answer the step leaves, whose scope the
	// replay keeps unpatched; "" patches at once.
	hold string
	// vm is the pick that names the MicroVM battery leases.
	vm string
}

// ctlSteps are the model's controller steps that are one reconcile of one
// claim, by name.
var ctlSteps = map[string]ctlStep{
	"ctlAddFinalizer":   {},
	"ctlReleasePending": {},
	// claim.ExpireDeleted and claim.Recover expire the claim from what the
	// controller remembers, and call nothing.
	"ctlExpireDeleted":  {},
	"ctlExpireUnlisted": {},

	"ctlClaimVM":           {mode: answer, calls: []string{methodClaimVM}, hold: claimAnswered, vm: "wv"},
	"ctlClaimVMLost":       {mode: lose, calls: []string{methodClaimVM}, vm: "wv"},
	"ctlClaimVMUnanswered": {mode: drop, calls: []string{methodClaimVM}},
	"ctlPoolExhausted":     {mode: answer, calls: []string{methodClaimVM}},

	"ctlHeartbeat":           {mode: answer, calls: []string{methodHeartbeat}, hold: heartbeatAnswered},
	"ctlHeartbeatLost":       {mode: lose, calls: []string{methodHeartbeat}},
	"ctlHeartbeatUnanswered": {mode: drop, calls: []string{methodHeartbeat}},
	"ctlHeartbeatUnknown":    {mode: answer, calls: []string{methodHeartbeat}},

	stepExpiryPassed:   {mode: answer, calls: []string{methodListLeases}},
	stepListRenewed:    {mode: answer, calls: []string{methodListLeases}},
	stepListUnanswered: {mode: drop, calls: []string{methodListLeases}},

	"ctlReleaseVM":         {mode: answer, calls: []string{methodReleaseVM}, hold: releaseAnswered},
	"ctlReleaseLost":       {mode: lose, calls: []string{methodReleaseVM}},
	"ctlReleaseUnanswered": {mode: drop, calls: []string{methodReleaseVM}},
}

// writeSteps are the model's steps that write an answer the controller
// holds: the patch of the scope a ctlStep kept.
var writeSteps = map[string]string{
	"ctlWriteBound":      claimAnswered,
	"ctlWriteRenewed":    heartbeatAnswered,
	"ctlRemoveFinalizer": releaseAnswered,
}

// modelSteps is every step of claims.qnt's step, which the testdata's
// traces must replay at least once between them.
var modelSteps = []string{
	stepCreateClaim, "deleteClaim", "renewClaim", "tick", "crashController",
	"batteryStop", "batteryStart", "dropEvent", "replenish", "batteryExpire",
	"ctlAddFinalizer", "ctlClaimVM", "ctlPoolExhausted", "ctlWriteBound",
	"ctlHeartbeat", "ctlHeartbeatUnknown", "ctlWriteRenewed", "ctlReleaseVM",
	"ctlRemoveFinalizer", "ctlReleasePending", stepRecover, stepEventDeleted,
	"ctlExpireDeleted", "ctlExpireUnlisted", stepExpiryPassed, stepListRenewed, "ctlClaimVMLost", "ctlClaimVMUnanswered",
	"ctlHeartbeatLost", "ctlHeartbeatUnanswered", stepListUnanswered,
	"ctlReleaseLost", "ctlReleaseUnanswered",
}

// divergence is a known difference between the model and the controller:
// a trace that reaches it stops being replayed there.
type divergence struct {
	name  string
	issue string
	// reaches reports whether step next, from state prev, reaches the
	// difference, and says how.
	reaches func(r *claimReplay, prev, next *modelState) (string, bool)
}

// knownDivergences are the differences between claims.qnt and the Claim
// Controller that the replay has found, each with its issue. There are
// none now: #132 to #136 brought the model in line with the controller. A
// difference found later goes here, with its issue.
var knownDivergences []divergence

// held is a scope the replay keeps unpatched: what the controller holds in
// memory between a call to battery and the status write.
type held struct {
	kind  string
	scope *claimscope.Scope
}

// claimReplay replays one trace.
type claimReplay struct {
	ctx   context.Context
	clock *clock.Fake
	kube  client.WithWatch
	bat   *scriptedBattery
	// r is the controller; a crash replaces it, and with it everything it
	// held in memory.
	r *MicroVMClaimReconciler
	// inflight is what the controller holds for each claim.
	inflight map[string]held
	// recovering is claims.qnt's ctrl.recovering.
	recovering bool
}

func newClaimReplay(t *testing.T) *claimReplay {
	t.Helper()
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The unit tests SHALL test each subreconciler, and the rest of
	//# the logic of the controllers and the Exec Agent, against the fake battery,
	//# the fake `flintlockd` and a fake Kubernetes client, without a Kubernetes
	//# API server.
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := batteryv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&batteryv1alpha1.MicroVMClaim{}).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, claim.NodeNameField, claim.NodeName).
		WithIndex(&batteryv1alpha1.MicroVMClaim{}, claim.MicroVMUIDIndex, func(o client.Object) []string {
			return claim.MicroVMUID(o.(*batteryv1alpha1.MicroVMClaim))
		}).
		Build()
	clk := clock.NewFake(replayStart)
	r := &claimReplay{
		ctx:      context.Background(),
		clock:    clk,
		kube:     kube,
		bat:      newScriptedBattery(clk),
		inflight: map[string]held{},
	}
	r.startController()
	r.recovering = false
	return r
}

// startController starts a new Claim Controller, as the Operator does
// after a crash: nothing the last one held in memory survives.
func (r *claimReplay) startController() {
	slots, err := claim.NewBindSlots(DefaultClaimConcurrentReconciles)
	if err != nil {
		panic(err)
	}
	r.r = &MicroVMClaimReconciler{
		Client:    r.kube,
		Scheme:    r.kube.Scheme(),
		APIReader: r.kube,
		Battery:   r.bat,
		Clock:     r.clock,
		Backoff:   claim.DefaultBackoff,
		slots:     slots,
		deleted:   &claim.DeletedVMs{Clock: r.clock},
		recovered: &claim.RecoveredLeases{},
	}
	r.inflight = map[string]held{}
	r.recovering = true
}

func replayKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: replayNamespace, Name: name}
}

// get reads claim name, or nil if the API server has none.
func (r *claimReplay) get(name string) (*batteryv1alpha1.MicroVMClaim, error) {
	c := &batteryv1alpha1.MicroVMClaim{}
	if err := r.kube.Get(r.ctx, replayKey(name), c); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

// reconcile runs one reconcile of claim name with chain: the scope, the
// chain, and the patch unless hold names what the controller keeps in
// memory instead. The chain must make exactly the calls calls.
func (r *claimReplay) reconcile(name string, chain claimscope.Chain, mode callMode, vm string, calls []string, hold string) error {
	if h, ok := r.inflight[name]; ok {
		return fmt.Errorf("claim %s is reconciled while the controller holds %s for it", name, h.kind)
	}
	c, err := r.get(name)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("claim %s is reconciled, and the API server has none", name)
	}
	s := r.r.scope(r.ctx, c)
	s.Log = logr.Discard()
	r.bat.begin(mode, vm)
	runErr := chain.Run(r.ctx, s)
	if !slices.Equal(r.bat.calls, calls) {
		return fmt.Errorf("claim %s: the chain called battery's %v, the model's step calls %v", name, r.bat.calls, calls)
	}
	if runErr != nil {
		return fmt.Errorf("claim %s: the chain failed: %w", name, runErr)
	}
	if hold != "" {
		r.inflight[name] = held{kind: hold, scope: s}
		return nil
	}
	return s.Patch(r.ctx)
}

// replayStep replays next, the step from prev.
func (r *claimReplay) replayStep(next *modelState) error {
	arg := next.pick
	switch a := next.Action; a {
	case stepCreateClaim:
		name, err := arg("u")
		if err != nil {
			return err
		}
		return r.kube.Create(r.ctx, &batteryv1alpha1.MicroVMClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: replayNamespace, Generation: 1},
			Spec: batteryv1alpha1.MicroVMClaimSpec{
				PoolRef:            batteryv1alpha1.PoolReference{Name: replayPool},
				ServiceAccountName: "holder",
			},
		})

	case "deleteClaim":
		name, err := arg("c")
		if err != nil {
			return err
		}
		c, err := r.get(name)
		if err != nil || c == nil {
			return fmt.Errorf("deleting claim %s: %v", name, err)
		}
		return r.kube.Delete(r.ctx, c)

	case "renewClaim":
		// The Holder patches spec.renewTime (RS-023).
		name, err := arg("c")
		if err != nil {
			return err
		}
		c, err := r.get(name)
		if err != nil || c == nil {
			return fmt.Errorf("renewing claim %s: %v", name, err)
		}
		n := renewCount(c.Spec.RenewTime) + 1
		c.Spec.RenewTime = &metav1.MicroTime{Time: renewBase.Add(time.Duration(n) * time.Second)}
		return r.kube.Update(r.ctx, c)

	case "tick":
		r.clock.Advance(replayTick)
		return nil

	case "crashController":
		r.startController()
		return nil

	case "batteryStop":
		r.bat.stop()
		return nil

	case "batteryStart":
		// The event watcher's next subscription succeeds, and recovery is
		// due (claimRecovery).
		r.bat.start()
		r.recovering = true
		return nil

	case "dropEvent":
		vm, err := arg("ev")
		if err != nil {
			return err
		}
		delete(r.bat.events, vm)
		return nil

	case "replenish":
		vm, err := arg("uv")
		if err != nil {
			return err
		}
		r.bat.replenish(vm)
		return nil

	case "batteryExpire":
		r.bat.sweep()
		return nil

	case stepEventDeleted:
		return r.eventDeleted(next)

	case stepRecover:
		return r.recover()
	}

	if st, ok := ctlSteps[next.Action]; ok {
		name, err := arg("c")
		if err != nil {
			return err
		}
		var vm string
		if st.vm != "" {
			if vm, err = arg(st.vm); err != nil {
				return err
			}
		}
		return r.reconcile(name, r.r.chain(), st.mode, vm, st.calls, st.hold)
	}

	if kind, ok := writeSteps[next.Action]; ok {
		name, err := arg("c")
		if err != nil {
			return err
		}
		h, ok := r.inflight[name]
		if !ok || h.kind != kind {
			return fmt.Errorf("%s for claim %s, and the controller holds %v for it", next.Action, name, h.kind)
		}
		delete(r.inflight, name)
		return h.scope.Patch(r.ctx)
	}

	return fmt.Errorf("the replay has no mapping for the model's step %s", next.Action)
}

// eventDeleted is ctlEventDeleted(ev): the event watcher receives one
// MicroVM deletion from battery's Events stream (claimEvents.handle), and
// each claim it sends is reconciled. Only the claims the model's step
// changes are reconciled here, the Bound claims on that MicroVM; any other
// the watcher sends is left to a later step for it, as the controller's
// work queue may.
func (r *claimReplay) eventDeleted(next *modelState) error {
	vm, err := next.pick("ev")
	if err != nil {
		return err
	}
	if !r.bat.up || !r.bat.events[vm] {
		return fmt.Errorf("the controller receives the deletion of %s, and battery's stream does not carry it", vm)
	}
	delete(r.bat.events, vm)
	out := make(chan event.GenericEvent, len(modelClaims))
	w := &claimEvents{Reader: r.kube, Deleted: r.r.deleted, Out: out, Log: logr.Discard()}
	w.handle(r.ctx, &battery.Event{VMUID: vm, Type: poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY})
	close(out)
	for e := range out {
		c := e.Object.(*batteryv1alpha1.MicroVMClaim)
		if _, ok := r.inflight[c.Name]; ok || !claim.HoldsLease(c) {
			continue
		}
		if err := r.reconcile(c.Name, r.r.chain(), answer, "", nil, ""); err != nil {
			return err
		}
	}
	return nil
}

// recover is ctlRecover: one recovery (claimRecovery.run), which reads the
// Leases with one ListLeases, and a reconcile of each claim it sends that
// is not in the middle of a step (#133). Each reconcile runs the whole
// chain (#136): claim.ExpireDeleted expires a claim whose MicroVM's
// deletion the controller has received; claim.Recover expires a claim
// whose Lease battery did not list, and otherwise takes battery's later
// expiry; claim.Renew relays a pending renewal with Heartbeat; and
// claim.CheckExpiry reads a Lease listed with a passed expiry again, and
// expires the claim (CL-014).
func (r *claimReplay) recover() error {
	if !r.recovering || !r.bat.up {
		return errors.New("the controller recovers, and no recovery is due")
	}
	out := make(chan event.GenericEvent, len(modelClaims))
	rec := &claimRecovery{Battery: r.bat, Reader: r.kube, Leases: r.r.recovered, Out: out, Log: logr.Discard()}
	r.bat.begin(answer, "")
	if err := rec.run(r.ctx); err != nil {
		return fmt.Errorf("recovering: %w", err)
	}
	if !slices.Equal(r.bat.calls, []string{methodListLeases}) {
		return fmt.Errorf("recovery called battery's %v, want one ListLeases", r.bat.calls)
	}
	close(out)
	for e := range out {
		c := e.Object.(*batteryv1alpha1.MicroVMClaim)
		if _, ok := r.inflight[c.Name]; ok {
			continue
		}
		var calls []string
		l, held := r.bat.leases[c.Status.LeaseID]
		switch {
		case c.Status.MicroVM != nil && r.r.deleted.Has(c.Status.MicroVM.UID), !held:
		case !c.Spec.RenewTime.Equal(c.Status.ObservedRenewTime):
			calls = []string{methodHeartbeat}
		case !l.ExpiresAt.After(r.clock.Now()):
			calls = []string{methodListLeases}
		}
		if err := r.reconcile(c.Name, r.r.chain(), answer, "", calls, ""); err != nil {
			return err
		}
	}
	r.recovering = false
	return nil
}

// renewCount is the model's renewTime of a spec.renewTime.
func renewCount(t *metav1.MicroTime) int64 {
	if t == nil {
		return 0
	}
	return int64(t.Sub(renewBase) / time.Second)
}

// claimView is a claim projected onto what claims.qnt's MicroVMClaim has.
type claimView struct {
	Slot              string
	RenewTime         int64
	ObservedRenewTime int64
	Finalizer         bool
	Deleting          bool
	Phase             string
	LeaseID           int64
	MicroVM           string
	NodeName          string
	BoundTime         string
	LeaseExpiresAt    string
	Bound             string
	Synced            string
}

// modelReasons maps types.qnt's reasons to the API's.
var modelReasons = map[string]string{
	"ClaimBound":         batteryv1alpha1.ReasonBound,
	"PoolExhausted":      batteryv1alpha1.ReasonPoolExhausted,
	"PoolNotFound":       batteryv1alpha1.ReasonPoolNotFound,
	"LeaseExpired":       batteryv1alpha1.ReasonLeaseExpired,
	"BatteryUnavailable": batteryv1alpha1.ReasonBatteryUnavailable,
	"BatteryAnswered":    batteryv1alpha1.ReasonSynced,
}

func modelCond(c modelCondition) string {
	switch c.Status.Tag {
	case "CondTrue":
		return "True/" + modelReasons[c.Reason.Tag]
	case "CondFalse":
		return "False/" + modelReasons[c.Reason.Tag]
	}
	return ""
}

func goCond(conds []metav1.Condition, typ string) string {
	c := meta.FindStatusCondition(conds, typ)
	if c == nil {
		return ""
	}
	return string(c.Status) + "/" + c.Reason
}

func optTick(o itfOption[itfInt]) string {
	if !o.Some {
		return ""
	}
	return fmt.Sprint(int64(o.V))
}

func goTick(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return fmt.Sprint(tickOf(t.Time))
}

// modelClaimView projects the model's claim.
func modelClaimView(slot string, c modelClaim) claimView {
	if slot != slotLive {
		return claimView{Slot: slot}
	}
	v := claimView{
		Slot:              slot,
		RenewTime:         int64(c.Spec.RenewTime),
		ObservedRenewTime: int64(c.ObservedRenewTime),
		Finalizer:         c.HasFinalizer,
		Deleting:          c.Deleting,
		Phase:             c.Status.Phase.Tag,
		MicroVM:           c.Status.MicroVM.V,
		NodeName:          c.Status.NodeName.V,
		BoundTime:         optTick(c.Status.BoundTime),
		LeaseExpiresAt:    optTick(c.Status.LeaseExpiresAt),
		Bound:             modelCond(c.Status.Bound),
		Synced:            modelCond(c.Status.Synced),
	}
	if c.Status.LeaseID.Some {
		v.LeaseID = int64(c.Status.LeaseID.V)
	}
	return v
}

// goClaimView projects the Go claim; nil is a claim the API server does not
// have, which is Gone once it has been created.
func goClaimView(c *batteryv1alpha1.MicroVMClaim, created bool) claimView {
	switch {
	case c == nil && created:
		return claimView{Slot: "Gone"}
	case c == nil:
		return claimView{Slot: "Unborn"}
	}
	st := c.Status
	v := claimView{
		Slot:              slotLive,
		RenewTime:         renewCount(c.Spec.RenewTime),
		ObservedRenewTime: renewCount(st.ObservedRenewTime),
		Finalizer:         controllerutil.ContainsFinalizer(c, batteryv1alpha1.ReleaseFinalizer),
		Deleting:          !c.DeletionTimestamp.IsZero(),
		Phase:             string(st.Phase),
		LeaseID:           leaseNum(st.LeaseID),
		BoundTime:         goTick(st.BoundTime),
		LeaseExpiresAt:    goTick(st.LeaseExpiresAt),
		Bound:             goCond(st.Conditions, batteryv1alpha1.ConditionBound),
		Synced:            goCond(st.Conditions, batteryv1alpha1.ConditionSynced),
	}
	if v.Phase == "" {
		// A claim no reconcile has written a phase to is Pending (RS-024).
		v.Phase = string(batteryv1alpha1.MicroVMClaimPending)
	}
	if st.MicroVM != nil {
		v.MicroVM = st.MicroVM.UID
	}
	if st.Host != nil {
		v.NodeName = st.Host.NodeName
	}
	return v
}

// leaseView is a Lease projected onto claims.qnt's.
type leaseView struct {
	VM        string
	Host      string
	ClaimedAt int64
	ExpiresAt int64
}

// batteryView is battery's state projected onto claims.qnt's.
type batteryView struct {
	Up        bool
	Warm      []string
	Events    []string
	Leases    map[int64]leaseView
	NextLease int64
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, ok := range m {
		if ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func modelBatteryView(b modelBattery) batteryView {
	v := batteryView{
		Up:        b.Up,
		Warm:      sortedKeys(b.Warm),
		Events:    sortedKeys(b.Events),
		Leases:    map[int64]leaseView{},
		NextLease: int64(b.NextLease),
	}
	for id, l := range b.Leases {
		v.Leases[int64(id)] = leaseView{VM: l.VM, Host: l.Host, ClaimedAt: int64(l.ClaimedAt), ExpiresAt: int64(l.ExpiresAt)}
	}
	return v
}

func (b *scriptedBattery) view() batteryView {
	v := batteryView{
		Up:        b.up,
		Warm:      sortedKeys(b.warm),
		Events:    sortedKeys(b.events),
		Leases:    map[int64]leaseView{},
		NextLease: b.nextLease,
	}
	for id, l := range b.leases {
		v.Leases[leaseNum(id)] = leaseView{
			VM:        l.VMUID,
			Host:      modelVMHost[l.VMUID],
			ClaimedAt: tickOf(l.ClaimedAt),
			ExpiresAt: tickOf(l.ExpiresAt),
		}
	}
	return v
}

// memView is what the controller holds for a claim, projected onto
// claims.qnt's InFlight.
func modelMemView(x itfVariant) (string, error) {
	switch x.Tag {
	case idle:
		return idle, nil
	case claimAnswered:
		var l modelLease
		err := x.into(&l)
		return fmt.Sprintf("%s(%d)", x.Tag, l.ID), err
	case heartbeatAnswered:
		var h modelHeartbeatAnswer
		err := x.into(&h)
		return fmt.Sprintf("%s(%d, expires %d, renewTime %d)", x.Tag, h.LeaseID, h.ExpiresAt, h.RenewTime), err
	case releaseAnswered:
		var l itfInt
		err := x.into(&l)
		return fmt.Sprintf("%s(%d)", x.Tag, l), err
	}
	return "", fmt.Errorf("an InFlight tagged %q", x.Tag)
}

func goMemView(h held, ok bool) string {
	if !ok {
		return idle
	}
	st := h.scope.Claim.Status
	switch h.kind {
	case heartbeatAnswered:
		var exp int64
		if st.LeaseExpiresAt != nil {
			exp = tickOf(st.LeaseExpiresAt.Time)
		}
		return fmt.Sprintf("%s(%d, expires %d, renewTime %d)", h.kind, leaseNum(st.LeaseID), exp, renewCount(st.ObservedRenewTime))
	case releaseAnswered:
		if controllerutil.ContainsFinalizer(h.scope.Claim, batteryv1alpha1.ReleaseFinalizer) {
			return h.kind + " with the finalizer kept"
		}
	}
	return fmt.Sprintf("%s(%d)", h.kind, leaseNum(st.LeaseID))
}

// compare compares the Go state with the model's state s, and describes
// every difference.
func (r *claimReplay) compare(s *modelState, created map[string]bool) ([]string, error) {
	var diffs []string
	if got, want := tickOf(r.clock.Now()), int64(s.Now); got != want {
		diffs = append(diffs, fmt.Sprintf("now: %d, the model's %d", got, want))
	}
	for _, name := range modelClaims {
		slot, mc, err := s.claim(name)
		if err != nil {
			return nil, err
		}
		c, err := r.get(name)
		if err != nil {
			return nil, err
		}
		if d := cmp.Diff(modelClaimView(slot, mc), goClaimView(c, created[name])); d != "" {
			diffs = append(diffs, fmt.Sprintf("claim %s (-model +Go):\n%s", name, d))
		}
		mem, err := modelMemView(s.Ctrl.Mem[name])
		if err != nil {
			return nil, err
		}
		h, ok := r.inflight[name]
		if got := goMemView(h, ok); got != mem {
			diffs = append(diffs, fmt.Sprintf("the controller holds %s for claim %s, the model's %s", got, name, mem))
		}
	}
	if d := cmp.Diff(modelBatteryView(s.Battery), r.bat.view()); d != "" {
		diffs = append(diffs, fmt.Sprintf("battery (-model +Go):\n%s", d))
	}
	if r.recovering != s.Ctrl.Recovering {
		diffs = append(diffs, fmt.Sprintf("recovery due: %t, the model's %t", r.recovering, s.Ctrl.Recovering))
	}
	// The deletions the controller remembers (#135).
	deleted := map[string]bool{}
	for vm := range modelVMHost {
		deleted[vm] = r.r.deleted.Has(vm)
	}
	if got, want := sortedKeys(deleted), sortedKeys(s.Ctrl.Deleted); !slices.Equal(got, want) {
		diffs = append(diffs, fmt.Sprintf("the controller remembers the deletion of %v, the model %v", got, want))
	}
	return diffs, nil
}

// stepName is a step with its picks, for messages.
func stepName(s *modelState) string {
	var picks []string
	for _, k := range []string{"c", "u", "wv", "uv", "ev"} {
		if v, err := s.pick(k); err == nil {
			picks = append(picks, k+"="+v)
		}
	}
	return s.Action + "{" + strings.Join(picks, " ") + "}"
}

// replayResult is what replaying one trace did.
type replayResult struct {
	// steps counts the steps replayed, by name.
	steps map[string]int
	// stopped is the known divergence the trace reached, if any.
	stopped string
}

// replayTrace replays tr, failing t at the first difference that is not a
// known divergence.
func replayTrace(t *testing.T, tr *itfTrace) replayResult {
	t.Helper()
	res := replayResult{steps: map[string]int{}}
	if len(tr.States) == 0 || tr.States[0].Action != "init" {
		t.Fatal("the trace does not start with init")
	}
	r := newClaimReplay(t)
	created := map[string]bool{}
	diffs, err := r.compare(&tr.States[0], created)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) > 0 {
		t.Fatalf("init differs:\n%s", strings.Join(diffs, "\n"))
	}
	for i := 1; i < len(tr.States); i++ {
		prev, next := &tr.States[i-1], &tr.States[i]
		for _, d := range knownDivergences {
			if how, ok := d.reaches(r, prev, next); ok {
				res.stopped = d.name
				t.Logf("step %d, %s: stopped at a known divergence, %s (%s): %s", i, stepName(next), d.name, d.issue, how)
				return res
			}
		}
		err := r.replayStep(next)
		if err == nil {
			if next.Action == stepCreateClaim {
				name, _ := next.pick("u")
				created[name] = true
			}
			diffs, err = r.compare(next, created)
		}
		if err != nil {
			t.Fatalf("step %d, %s: %v", i, stepName(next), err)
		}
		if len(diffs) > 0 {
			t.Fatalf("step %d, %s: the controller and the model differ:\n%s", i, stepName(next), strings.Join(diffs, "\n"))
		}
		res.steps[next.Action]++
	}
	return res
}

// TestClaimControllerReplaysTheModelsTraces replays every trace of
// claims.qnt in testdata/claims-traces against the Claim Controller (#62),
// and checks that the traces between them replay every step of the model.
func TestClaimControllerReplaysTheModelsTraces(t *testing.T) {
	glob := os.Getenv("BATTERY_OPERATOR_CLAIMS_TRACES")
	fromTestdata := glob == ""
	if fromTestdata {
		glob = filepath.Join("testdata", "claims-traces", "*.itf.json.gz")
	}
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no traces match %s: run make claims-traces", glob)
	}
	replayed := map[string]int{}
	stopped := map[string]int{}
	for _, p := range paths {
		tr, err := readTrace(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(p), func(t *testing.T) {
			res := replayTrace(t, tr)
			for a, n := range res.steps {
				replayed[a] += n
			}
			if res.stopped != "" {
				stopped[res.stopped]++
			}
		})
	}
	t.Logf("replayed %d traces; steps replayed: %v; traces stopped at a known divergence: %v", len(paths), replayed, stopped)
	if !fromTestdata || t.Failed() {
		return
	}
	for _, a := range modelSteps {
		if replayed[a] == 0 {
			t.Errorf("no trace in testdata replays the model's step %s: generate more with make claims-traces", a)
		}
	}
}
