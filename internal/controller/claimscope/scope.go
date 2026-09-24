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

// Package claimscope holds the Claim Controller's per-reconcile scope, the
// subreconciler interface and the chain that runs them, as CLAUDE.md's
// "Controller structure" describes.
//
// These are local stand-ins for the shared Scope[T], subreconciler and
// chain that #58 brings. They have the same shape, specialised to
// MicroVMClaim, so that moving the Claim Controller onto the shared types
// changes no logic: Scope becomes Scope[*MicroVMClaim] plus the claim's own
// field (BatteryCall), and Subreconciler, Func, Result and Chain keep their
// meaning.
package claimscope

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
)

// Scope is one reconcile of one MicroVMClaim. Subreconcilers change Claim
// and Result and never write to the API server; the controller calls Patch
// once, after the chain, to write what they changed.
type Scope struct {
	// Claim is the working copy the subreconcilers change.
	Claim *batteryv1alpha1.MicroVMClaim
	// Original is the claim as it was fetched. Patch diffs Claim against it.
	Original *batteryv1alpha1.MicroVMClaim

	// Client is the Kubernetes client, reading from the manager's cache.
	Client client.Client
	// APIReader reads from the API server directly, bypassing the cache.
	APIReader client.Reader
	// Battery is the Operator's battery client.
	Battery battery.Client
	// Log is the reconcile's logger, with the claim's key on it.
	Log logr.Logger
	// Clock is the time source for every time the subreconcilers write.
	Clock clock.Clock

	// Result is the chain's result so far.
	Result Result

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

// New builds a scope for claim, keeping a copy of it as fetched.
func New(claim *batteryv1alpha1.MicroVMClaim) *Scope {
	return &Scope{Claim: claim, Original: claim.DeepCopy()}
}

// Result is what a subreconciler asks of the chain.
type Result struct {
	// Stop ends the chain's Steps after this subreconciler. Its Finally
	// subreconcilers still run, and the scope is still patched.
	Stop bool
	// RequeueAfter, when positive, asks for another reconcile after this
	// long.
	RequeueAfter time.Duration
}

// merge folds r into the result so far: the chain stops if either asks it
// to, and the shortest positive RequeueAfter wins.
func (r Result) merge(o Result) Result {
	out := Result{Stop: r.Stop || o.Stop, RequeueAfter: r.RequeueAfter}
	if o.RequeueAfter > 0 && (out.RequeueAfter <= 0 || o.RequeueAfter < out.RequeueAfter) {
		out.RequeueAfter = o.RequeueAfter
	}
	return out
}

// Subreconciler is one concern of the Claim Controller.
type Subreconciler interface {
	// Reconcile changes the scope and says whether the chain continues,
	// requeues or stops. An error stops the chain.
	Reconcile(ctx context.Context, s *Scope) (Result, error)
}

// Func adapts a function to Subreconciler.
type Func func(ctx context.Context, s *Scope) (Result, error)

// Reconcile implements Subreconciler.
func (f Func) Reconcile(ctx context.Context, s *Scope) (Result, error) { return f(ctx, s) }

// Chain is a controller's subreconcilers.
type Chain struct {
	// Steps run in order until one stops the chain or fails.
	Steps []Subreconciler
	// Finally run after Steps however they ended, and all of them run:
	// they report on what the Steps did, such as a condition that
	// summarises it.
	Finally []Subreconciler
}

// Run runs the chain on s, folding each result into s.Result, and returns
// the errors of the step that failed and of any Finally subreconciler.
func (c Chain) Run(ctx context.Context, s *Scope) error {
	var errs []error
	for _, sub := range c.Steps {
		res, err := sub.Reconcile(ctx, s)
		s.Result = s.Result.merge(res)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if res.Stop {
			break
		}
	}
	for _, sub := range c.Finally {
		res, err := sub.Reconcile(ctx, s)
		// A Finally subreconciler cannot stop what has already run.
		res.Stop = false
		s.Result = s.Result.merge(res)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Patch writes what the chain changed: the claim's metadata, then its
// status, each only if it changed.
//
// The metadata patch carries an optimistic lock, because a merge patch
// replaces the whole finalizer list and a stale copy would drop another
// controller's finalizer. The status patch carries none, so that a
// concurrent change elsewhere in the claim never makes the status write
// fail: only the Claim Controller writes status, and a status that records
// a Lease must not be lost to a conflict.
func (s *Scope) Patch(ctx context.Context) error {
	desired := s.Claim.Status.DeepCopy()

	// The metadata patch leaves status out: the status subresource is
	// written on its own below.
	s.Claim.Status = *s.Original.Status.DeepCopy()
	changed, err := changes(client.MergeFrom(s.Original), s.Claim)
	if err != nil {
		s.Claim.Status = *desired
		return err
	}
	if changed {
		// A deleted claim whose last finalizer this patch removes is gone
		// once it is written, and has no status left to write.
		gone := !s.Claim.DeletionTimestamp.IsZero() && len(s.Claim.Finalizers) == 0
		// The API server's answer overwrites the claim, so the status the
		// chain wants is put back afterwards.
		meta := client.MergeFromWithOptions(s.Original, client.MergeFromWithOptimisticLock{})
		if err := s.Client.Patch(ctx, s.Claim, meta); err != nil {
			s.Claim.Status = *desired
			if gone && apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("patching MicroVMClaim %s: %w", client.ObjectKeyFromObject(s.Claim), err)
		}
		if gone {
			s.Claim.Status = *desired
			return nil
		}
	}

	base := s.Claim.DeepCopy()
	s.Claim.Status = *desired
	st := client.MergeFrom(base)
	changed, err = changes(st, s.Claim)
	if err != nil {
		return err
	}
	if changed {
		if err := s.Client.Status().Patch(ctx, s.Claim, st); err != nil {
			return fmt.Errorf("patching the status of MicroVMClaim %s: %w", client.ObjectKeyFromObject(s.Claim), err)
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
