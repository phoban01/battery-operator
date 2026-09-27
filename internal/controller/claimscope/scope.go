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

// Package claimscope holds the Claim Controller's per-reconcile scope, as
// CLAUDE.md's "Controller structure" describes. The scope embeds the
// shared reconcile.Scope[*MicroVMClaim] and adds the claim's own fields;
// the subreconcilers and their chain are internal/reconcile's
// SubReconciler, Func, Result and Chain over *Scope.
package claimscope

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// Scope is one reconcile of one MicroVMClaim. Subreconcilers change Object
// and never write to the API server; the controller calls Patch once,
// after the chain, to write what they changed.
type Scope struct {
	*reconcile.Scope[*batteryv1alpha1.MicroVMClaim]

	// APIReader reads from the API server directly, bypassing the cache.
	APIReader client.Reader
	// Battery is the Operator's battery client.
	Battery battery.Client

	// BatteryCall is the last call to battery the chain made for the claim
	// in this reconcile, or nil if it made none.
	BatteryCall *BatteryCall
}

// BatteryCall is a call to battery and how it ended.
type BatteryCall struct {
	// Method is the Client method, such as "ClaimVM".
	Method string
	// Err is the call's error, nil when it succeeded.
	Err error
}

// Called records a call to battery in the scope.
func (s *Scope) Called(method string, err error) {
	s.BatteryCall = &BatteryCall{Method: method, Err: err}
}

// New builds a scope for claim, keeping a copy of it as fetched. The
// caller sets the clients, the logger and the clock; the clock is the
// wall clock until it does.
func New(claim *batteryv1alpha1.MicroVMClaim) *Scope {
	return &Scope{Scope: reconcile.NewScope(claim, nil, logr.Discard(), nil)}
}

// Patch writes what the chain changed: the claim's metadata, then its
// status, each only if it changed. It is the Claim Controller's own write,
// in place of reconcile.Scope's Patch.
//
// The metadata patch carries an optimistic lock, because a merge patch
// replaces the whole finalizer list and a stale copy would drop another
// controller's finalizer. The status patch carries none, so that a
// concurrent change elsewhere in the claim never makes the status write
// fail: only the Claim Controller writes status, and a status that records
// a Lease must not be lost to a conflict.
func (s *Scope) Patch(ctx context.Context) error {
	desired := s.Object.Status.DeepCopy()

	// The metadata patch leaves status out: the status subresource is
	// written on its own below.
	s.Object.Status = *s.Original.Status.DeepCopy()
	changed, err := changes(client.MergeFrom(s.Original), s.Object)
	if err != nil {
		s.Object.Status = *desired
		return err
	}
	if changed {
		// A deleted claim whose last finalizer this patch removes is gone
		// once it is written, and has no status left to write.
		gone := !s.Object.DeletionTimestamp.IsZero() && len(s.Object.Finalizers) == 0
		// The API server's answer overwrites the claim, so the status the
		// chain wants is put back afterwards.
		meta := client.MergeFromWithOptions(s.Original, client.MergeFromWithOptimisticLock{})
		if err := s.Client.Patch(ctx, s.Object, meta); err != nil {
			s.Object.Status = *desired
			if gone && apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("patching MicroVMClaim %s: %w", client.ObjectKeyFromObject(s.Object), err)
		}
		if gone {
			s.Object.Status = *desired
			return nil
		}
	}

	base := s.Object.DeepCopy()
	s.Object.Status = *desired
	st := client.MergeFrom(base)
	changed, err = changes(st, s.Object)
	if err != nil {
		return err
	}
	if changed {
		if err := s.Client.Status().Patch(ctx, s.Object, st); err != nil {
			return fmt.Errorf("patching the status of MicroVMClaim %s: %w", client.ObjectKeyFromObject(s.Object), err)
		}
	}
	return nil
}

// changes reports whether p has anything to say about obj.
func changes(p client.Patch, obj client.Object) (bool, error) {
	data, err := p.Data(obj)
	if err != nil {
		return false, fmt.Errorf("computing a patch of MicroVMClaim %s: %w", client.ObjectKeyFromObject(obj), err)
	}
	return string(data) != "{}", nil
}
