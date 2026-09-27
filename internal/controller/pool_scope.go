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
	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/battery"
	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// poolScope is one reconcile of one Pool. Subreconcilers change the Pool
// and never write to the API server; the Pool Controller writes the Pool
// once, at the end, with the embedded Scope's Patch.
type poolScope struct {
	*reconcile.Scope[*batteryv1alpha1.Pool]

	Battery battery.Client
	// Hosts resolves the Pool's selector to the Hosts it matches.
	Hosts PoolHosts

	// refusal is battery's refusal of the Pool's spec in this reconcile,
	// from CreatePool or UpdatePool, wrapping battery.ErrInvalid.
	refusal error

	// held is the Pool as battery last answered for it in this reconcile,
	// from GetPool, CreatePool or UpdatePool; nil while battery does not
	// hold the Pool. The status subreconcilers read battery's PoolStatus
	// from it (PO-020).
	held *battery.Pool

	// hosts are the names, sorted, of the Hosts the Pool's selector
	// matches, as poolPlacement resolved them in this reconcile: the
	// Pool's flintlock_hosts in battery (PO-010).
	hosts []string

	// sent is set once poolDeclaration or poolUpdate has sent the Pool's
	// spec to battery in this reconcile, which starts battery's reconciler
	// for the Pool again (BA-074).
	sent bool

	// notFilling is set by poolReseed when a reseed has not filled the
	// Pool, for poolReadiness (PO-039).
	notFilling *poolNotFilling

	// Reseeds is the Pool Controller's memory of its stalled Pools, for
	// poolReseed; nil turns the reseed off.
	Reseeds *poolReseeds
}

func newPoolScope(pool *batteryv1alpha1.Pool, c client.Client, b battery.Client, hosts PoolHosts,
	log logr.Logger, clk clock.Clock) *poolScope {
	return &poolScope{
		Scope:   reconcile.NewScope(pool, c, log, clk),
		Battery: b,
		Hosts:   hosts,
	}
}

// setCondition sets one of the Pool's conditions for its current
// generation, stamping a transition with the scope's clock.
func (s *poolScope) setCondition(conditionType string, status metav1.ConditionStatus, reason, message string) {
	reconcile.SetCondition(&s.Object.Status.Conditions, s.Object, s.Clock, conditionType, status, reason, message)
}
