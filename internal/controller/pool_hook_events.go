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

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
)

// The Kubernetes Event the Pool Controller records on a Pool for each
// VM_HOOK_FAILED (PO-041).
const (
	// PoolEventReasonHookFailed is the Event's reason.
	PoolEventReasonHookFailed = "HookFailed"
	// poolEventActionRunHook is the Event's action: battery ran a hook on
	// one of the Pool's MicroVMs.
	poolEventActionRunHook = "RunHook"
)

// poolHookEvents records a Kubernetes Event on a Pool for each
// VM_HOOK_FAILED that battery reports for it (PO-041). It is the one
// failure in provisioning that battery v0.3.3 reports; a CreateMicroVM
// that flintlockd refuses records no event. Refactor it, with the
// NotFilling reason of PO-039, once battery reports why a provision
// fails.
//
// battery replays its outbox on each subscription (BA-050), so
// poolHookEvents remembers the last event id it recorded for each Pool,
// by the Pool's uid, and records no event twice. A Pool deleted and
// created again has a new uid, and starts again.
type poolHookEvents struct {
	// Pools reads the Pool an event names, for the Event's object.
	Pools client.Reader
	// Recorder records the Events.
	Recorder events.EventRecorder
	// Log is the logger; the zero Logger discards.
	Log logr.Logger

	mu       sync.Mutex
	recorded map[types.UID]int64
}

// hookFailedPayload is the payload_json of a VM_HOOK_FAILED as the fake
// battery writes it. battery v0.3.3 writes none.
type hookFailedPayload struct {
	Hook  string `json:"hook"`
	Error string `json:"error"`
}

// handle records the Event for ev, when ev is a VM_HOOK_FAILED it has not
// recorded yet.
func (h *poolHookEvents) handle(ctx context.Context, ev *battery.Event) {
	if h == nil || h.Recorder == nil || ev.Type != poolmgrv1.EventType_VM_HOOK_FAILED || ev.Pool.Name == "" {
		return
	}
	pool := &batteryv1alpha1.Pool{}
	key := client.ObjectKey{Namespace: ev.Pool.Namespace, Name: ev.Pool.Name}
	if err := h.Pools.Get(ctx, key, pool); err != nil {
		if !apierrors.IsNotFound(err) {
			h.Log.Error(err, "Failed to get Pool to record a failed hook", "pool", ev.Pool.String())
		}
		return
	}
	if !h.first(pool.UID, ev.ID) {
		return
	}

	//= docs/requirements/03-pools.md#pool-status
	//# When battery's `Events` stream reports a `VM_HOOK_FAILED`
	//# event for a Pool, the Pool Controller SHALL record on the Pool a
	//# Kubernetes Event of type `Warning` with the reason `HookFailed`, whose
	//# message names the MicroVM and, where the event's payload gives them, the
	//# hook and its error.
	h.Recorder.Eventf(pool, nil, corev1.EventTypeWarning, PoolEventReasonHookFailed, poolEventActionRunHook,
		"%s", hookFailedNote(ev))
}

// first reports whether id is past the last event id recorded for the
// Pool of uid, and remembers it when it is.
func (h *poolHookEvents) first(uid types.UID, id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.recorded == nil {
		h.recorded = map[types.UID]int64{}
	}
	if last, ok := h.recorded[uid]; ok && id <= last {
		return false
	}
	h.recorded[uid] = id
	return true
}

// hookFailedNote is the Event's message: the MicroVM, and the hook and
// its error where the payload gives them.
func hookFailedNote(ev *battery.Event) string {
	var p hookFailedPayload
	if len(ev.Payload) > 0 {
		_ = json.Unmarshal(ev.Payload, &p)
	}
	hook := "A hook"
	if p.Hook != "" {
		hook = fmt.Sprintf("The %s hook", p.Hook)
	}
	msg := fmt.Sprintf("%s failed on MicroVM %s", hook, ev.VMUID)
	if p.Error != "" {
		return msg + ": " + p.Error
	}
	return msg + "; battery does not report why: see the log of the battery container"
}
