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

// Package reconcile holds the shared building blocks of the Operator's
// controllers, as CLAUDE.md's "Controller structure" describes: a scope
// for one reconcile of one object (Scope), the subreconciler interface
// (SubReconciler), a chain that runs subreconcilers in order (Chain), and
// generic subreconcilers and helpers that serve any controller.
//
// They are extracted from the local types the Claim, Pool and Inventory
// Controllers were written with (claimscope, poolScope, inventory.Scope),
// which keep theirs for now.
//
// A controller fetches its object, builds its scope (usually a struct that
// embeds *Scope[T] and adds the controller's own fields), runs its Chain
// on it, and then writes the object once.
package reconcile

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Result is what a subreconciler asks of the chain.
type Result struct {
	// Stop ends the chain's Steps after this subreconciler. Its Finally
	// subreconcilers still run, and the controller still writes the scope.
	Stop bool
	// RequeueAfter, when positive, asks for another reconcile after this
	// long.
	RequeueAfter time.Duration
}

// Merge folds o into the result so far: the chain stops if either asks it
// to, and the shortest positive RequeueAfter wins.
func (r Result) Merge(o Result) Result {
	out := Result{Stop: r.Stop || o.Stop, RequeueAfter: r.RequeueAfter}
	if o.RequeueAfter > 0 && (out.RequeueAfter <= 0 || o.RequeueAfter < out.RequeueAfter) {
		out.RequeueAfter = o.RequeueAfter
	}
	return out
}

// Ctrl is the result as controller-runtime takes it.
func (r Result) Ctrl() ctrl.Result {
	return ctrl.Result{RequeueAfter: r.RequeueAfter}
}

// SubReconciler is one concern of a controller, over its scope type S:
// *Scope[T], or a controller's own scope that embeds one.
type SubReconciler[S any] interface {
	// Reconcile changes the scope and says whether the chain continues,
	// requeues or stops. An error stops the chain. It never writes the
	// scope's object to the API server: the controller does, once, after
	// the chain.
	Reconcile(ctx context.Context, s S) (Result, error)
}

// Func adapts a function to SubReconciler.
type Func[S any] func(ctx context.Context, s S) (Result, error)

// Reconcile implements SubReconciler.
func (f Func[S]) Reconcile(ctx context.Context, s S) (Result, error) { return f(ctx, s) }

// Chain is a controller's subreconcilers. A Chain is itself a
// SubReconciler, so one chain can be a step of another: a group of checks,
// for example, that stops at the first that fails without stopping the
// chain around it (see Group).
type Chain[S any] struct {
	// Steps run in order until one stops the chain or fails.
	Steps []SubReconciler[S]
	// Finally run after Steps however they ended, and all of them run:
	// they report on what the Steps did, such as a condition that
	// summarises it. A Finally subreconciler cannot stop what has already
	// run.
	Finally []SubReconciler[S]
}

// Steps is a Chain of steps alone, with nothing to run finally.
func Steps[S any](steps ...SubReconciler[S]) Chain[S] {
	return Chain[S]{Steps: steps}
}

// Run runs the chain on s. It returns every result folded together, and
// the errors of the step that failed and of any Finally subreconciler.
func (c Chain[S]) Run(ctx context.Context, s S) (Result, error) {
	var out Result
	var errs []error
	for _, sub := range c.Steps {
		res, err := sub.Reconcile(ctx, s)
		out = out.Merge(res)
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
		res.Stop = false
		out = out.Merge(res)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// Reconcile implements SubReconciler: it runs the chain, and its result,
// Stop included, is the chain's.
func (c Chain[S]) Reconcile(ctx context.Context, s S) (Result, error) {
	return c.Run(ctx, s)
}

// Group runs a chain as one step of another, and keeps its Stop to itself:
// a step of the group that stops ends the group, and the chain around it
// goes on. Its RequeueAfter and errors still count.
type Group[S any] struct {
	Chain[S]
}

// Reconcile implements SubReconciler.
func (g Group[S]) Reconcile(ctx context.Context, s S) (Result, error) {
	res, err := g.Run(ctx, s)
	res.Stop = false
	return res, err
}
