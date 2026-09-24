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

package reconcile

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/clock"
)

// Scope is one reconcile of one object. Subreconcilers change Object and
// never write to the API server; the controller writes what they changed
// once, after the chain: with Patch, or with a write of its own where the
// object's API calls for one.
//
// A controller whose subreconcilers need more than this embeds *Scope[T]
// in a scope of its own, with its own fields beside it.
type Scope[T client.Object] struct {
	// Object is the working copy the subreconcilers change.
	Object T
	// Original is the object as it was fetched, the base of the write.
	Original T

	// Client is the Kubernetes client.
	Client client.Client
	// Log is the reconcile's logger.
	Log logr.Logger
	// Clock is the time source for every time the subreconcilers write.
	Clock clock.Clock
}

// NewScope builds a scope for obj, keeping a copy of it as fetched. A nil
// clock is the wall clock.
func NewScope[T client.Object](obj T, c client.Client, log logr.Logger, clk clock.Clock) *Scope[T] {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Scope[T]{
		Object:   obj,
		Original: obj.DeepCopyObject().(T),
		Client:   c,
		Log:      log,
		Clock:    clk,
	}
}

// Base returns the scope itself, so that a controller's own scope, which
// embeds *Scope[T], gives the generic subreconcilers its object (Scoped).
func (s *Scope[T]) Base() *Scope[T] { return s }

// Scoped is a scope with an object of type T: *Scope[T], or a controller's
// own scope that embeds one.
type Scoped[T client.Object] interface {
	Base() *Scope[T]
}

// Patch writes what the subreconcilers changed with merge patches against
// the object as fetched: the status first, through the status subresource,
// then the rest (metadata and spec), each only if it changed. The status
// goes first because removing the last finalizer lets the API server
// delete the object. An object already gone is not an error.
//
// This is the Pool Controller's patch, made generic. A controller whose
// object needs another write (an optimistic lock, a subresource of its
// own, an update rather than a patch) writes it itself.
func (s *Scope[T]) Patch(ctx context.Context) error {
	orig, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s.Original)
	if err != nil {
		return fmt.Errorf("converting %s: %w", client.ObjectKeyFromObject(s.Original), err)
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s.Object)
	if err != nil {
		return fmt.Errorf("converting %s: %w", client.ObjectKeyFromObject(s.Object), err)
	}
	statusChanged := !equality.Semantic.DeepEqual(orig["status"], obj["status"])
	delete(orig, "status")
	delete(obj, "status")
	restChanged := !equality.Semantic.DeepEqual(orig, obj)

	base := client.MergeFrom(s.Original)
	var errs []error
	if statusChanged {
		if err := s.Client.Status().Patch(ctx, s.Object.DeepCopyObject().(T), base); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("patching the status of %s: %w", client.ObjectKeyFromObject(s.Object), err))
		}
	}
	if restChanged {
		if err := s.Client.Patch(ctx, s.Object.DeepCopyObject().(T), base); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("patching %s: %w", client.ObjectKeyFromObject(s.Object), err))
		}
	}
	return errors.Join(errs...)
}
