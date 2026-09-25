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
	"fmt"
	"maps"
	"net"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/internal/reconcile"
)

// AddressPinsConfigMap is the ConfigMap, in the Operator's namespace, that
// pins each Node's address: one entry for each Node name, whose value is the
// address the Operator first signed a serving certificate for. The RBAC
// markers grant write access to this name only.
const AddressPinsConfigMap = "host-address-pins"

// ReasonAddressPinned: the request's address is not the one pinned to its
// Node, or is pinned to another Node.
const ReasonAddressPinned = "AddressPinned"

// addressPins is the pins ConfigMap as one reconcile read it, indexed both
// ways.
type addressPins struct {
	// configMap is the ConfigMap as read; nil while there is none.
	configMap *corev1.ConfigMap
	// byAddress is the Nodes each address is pinned to, by the address's
	// canonical form. More than one only if an administrator wrote it so.
	byAddress map[string][]string
}

// newAddressPins indexes cm, which is nil when there is no ConfigMap.
func newAddressPins(cm *corev1.ConfigMap) *addressPins {
	p := &addressPins{configMap: cm, byAddress: map[string][]string{}}
	if cm == nil {
		return p
	}
	for _, node := range slices.Sorted(maps.Keys(cm.Data)) {
		a := canonicalAddress(cm.Data[node])
		p.byAddress[a] = append(p.byAddress[a], node)
	}
	return p
}

// canonicalAddress is a's canonical form when it parses as an IP address,
// and a unchanged otherwise, so that an entry nobody can match stays one.
func canonicalAddress(a string) string {
	if ip := net.ParseIP(a); ip != nil {
		return ip.String()
	}
	return a
}

// pinned returns the address pinned to node, and whether there is one.
func (p *addressPins) pinned(node string) (string, bool) {
	if p.configMap == nil {
		return "", false
	}
	a, ok := p.configMap.Data[node]
	return a, ok
}

// pinIndex keeps the index of the pins ConfigMap for the version the cache
// last returned, so that finding the Nodes an address is pinned to does not
// walk every entry for every request.
type pinIndex struct {
	mu      sync.Mutex
	uid     types.UID
	version string
	pins    *addressPins
}

// of returns the index of cm, building it only for a version it has not
// seen. cm is nil when there is no ConfigMap.
func (x *pinIndex) of(cm *corev1.ConfigMap) *addressPins {
	if x == nil || cm == nil || cm.ResourceVersion == "" {
		return newAddressPins(cm)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pins == nil || x.uid != cm.UID || x.version != cm.ResourceVersion {
		x.uid, x.version, x.pins = cm.UID, cm.ResourceVersion, newAddressPins(cm)
	}
	return x.pins
}

// readPins reads the pins ConfigMap through r, and indexes it through x.
func readPins(ctx context.Context, r client.Reader, x *pinIndex, namespace string) (*addressPins, error) {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: namespace, Name: AddressPinsConfigMap}
	if err := r.Get(ctx, key, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("reading ConfigMap %s: %w", key, err)
		}
		cm = nil
	}
	return x.of(cm), nil
}

// csrAddressPin checks a serving request's address against the pins
// (CT-015), and records in the scope the pin the scope's write stores
// before the certificate (CT-016) when the Node has none yet. It comes
// after csrSubjectAltNames, which has checked that the request names
// exactly one address, one of its Node's.
type csrAddressPin struct{}

// Reconcile implements reconcile.SubReconciler.
func (csrAddressPin) Reconcile(ctx context.Context, s *csrScope) (reconcile.Result, error) {
	signer := s.Object.Spec.SignerName
	if signer != hostcert.ServingSigner && signer != hostcert.ExecAgentServingSigner {
		return reconcile.Result{}, nil
	}
	pins, err := readPins(ctx, s.pins, s.pinIndex, s.Config.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	addr := s.req.IPAddresses[0].String()

	//= docs/requirements/09-certificates.md#approval
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request only when its IP
	//# address is the address pinned to the requester's Node or, when no address
	//# is pinned to that Node, an address pinned to no other Node.
	if pinned, ok := pins.pinned(s.node); ok {
		if canonicalAddress(pinned) != addr {
			return s.deny(denyf(ReasonAddressPinned,
				"The Node %q's address is pinned to %s, not %s; an administrator clears the pin in ConfigMap %s/%s when the Host changes address",
				s.node, pinned, addr, s.Config.Namespace, AddressPinsConfigMap))
		}
		return reconcile.Result{}, nil
	}
	if others := pins.byAddress[addr]; len(others) > 0 {
		return s.deny(denyf(ReasonAddressPinned,
			"The address %s is pinned to the Node %q; an administrator clears the pin in ConfigMap %s/%s when the address moves",
			addr, others[0], s.Config.Namespace, AddressPinsConfigMap))
	}

	//= docs/requirements/09-certificates.md#approval
	//# The Operator SHALL keep the pinned addresses in the ConfigMap
	//# `host-address-pins` in its namespace, one entry for each Node name, and
	//# SHALL NOT change or remove an entry, even when its Node is deleted.
	//
	// The only change the Operator makes to the ConfigMap is this new
	// entry, for a Node that has none; nothing else in it writes the
	// ConfigMap, and nothing watches Nodes to remove one.
	var cm *corev1.ConfigMap
	if pins.configMap == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Namespace: s.Config.Namespace,
			Name:      AddressPinsConfigMap,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "battery-operator"},
		}}
	} else {
		cm = pins.configMap.DeepCopy()
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[s.node] = addr
	s.pin = cm
	return reconcile.Result{}, nil
}

// writePin stores the scope's new pin: it creates the ConfigMap while there
// is none, and otherwise updates it with the resourceVersion the check read,
// so that the API server refuses the write if anything changed the pins
// since. A refused write fails the reconcile, which is retried against the
// pins as they are then.
func (s *csrScope) writePin(ctx context.Context) error {
	cm, key := s.pin, client.ObjectKeyFromObject(s.pin)
	if cm.ResourceVersion == "" {
		if err := s.Client.Create(ctx, cm); err != nil {
			return fmt.Errorf("creating ConfigMap %s: %w", key, err)
		}
	} else if err := s.Client.Update(ctx, cm); err != nil {
		return fmt.Errorf("updating ConfigMap %s: %w", key, err)
	}
	s.Log.Info("Pinned the Node's address", "node", s.node, "address", cm.Data[s.node], "configMap", key)
	return nil
}
