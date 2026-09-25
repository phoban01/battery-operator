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
	"errors"
	"maps"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// pinsKey is the pins ConfigMap's key in the tests.
var pinsKey = client.ObjectKey{Namespace: operatorNamespace, Name: AddressPinsConfigMap}

// pins returns the pins as stored, or nil while there is no ConfigMap.
func (s *signer) pins(t *testing.T) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := s.c.Get(context.Background(), pinsKey, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	if cm.Data == nil {
		return map[string]string{}
	}
	return cm.Data
}

// setPins replaces the stored pins with data, as an administrator would.
func (s *signer) setPins(t *testing.T, data map[string]string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{}
	if err := s.c.Get(ctx, pinsKey, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data = data
	if err := s.c.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
}

// setAddresses sets node's internal addresses, as its kubelet would.
func (s *signer) setAddresses(t *testing.T, node string, ips ...string) {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Node{}
	if err := s.c.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
		t.Fatal(err)
	}
	n.Status.Addresses = testNode(node, ips...).Status.Addresses
	if err := s.c.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
}

// wantRefused fails t unless the request name, reconciled, is denied, or
// marked Failed when approved, for ReasonAddressPinned, and not signed.
func (s *signer) wantRefused(t *testing.T, name string) {
	t.Helper()
	csr := s.reconcile(t, name)
	cond := condition(csr, certificatesv1.CertificateDenied)
	if cond == nil {
		cond = condition(csr, certificatesv1.CertificateFailed)
	}
	if cond == nil || cond.Reason != ReasonAddressPinned {
		t.Errorf("conditions = %+v, want Denied or Failed for %s", csr.Status.Conditions, ReasonAddressPinned)
	}
	if len(csr.Status.Certificate) != 0 {
		t.Error("a request for an address that is not its Node's pin was signed")
	}
}

func TestSignerPinsTheFirstAddressItSignsFor(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# When the Operator signs a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request for a Node that has
	//# no pinned address, the Operator SHALL pin the request's IP address to that
	//# Node, and SHALL store the certificate only once the pin is stored.

	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL keep the pinned addresses in the ConfigMap
	//# `host-address-pins` in its namespace, one entry for each Node name, and
	//# SHALL NOT change or remove an entry, even when its Node is deleted.

	for _, tc := range []struct {
		name    string
		request func(node, ip string) request
	}{
		{"flintlockd-serving", servingRequest},
		{"exec-agent-serving", execAgentServingRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			if s.pins(t) != nil {
				t.Fatal("the pins ConfigMap exists before any request")
			}
			// The client certificate names no address and pins none.
			s.issue(t, nodeA, clientRequest(nodeA))
			if s.pins(t) != nil {
				t.Errorf("pins = %v after a client certificate, want no ConfigMap", s.pins(t))
			}

			// The dual-stack Node's first serving certificate pins its IPv6
			// address; the other Node's pins its own, beside it.
			s.issue(t, nodeA, tc.request(nodeA, nodeAIPv6))
			s.issue(t, nodeB, tc.request(nodeB, nodeBIP))
			want := map[string]string{nodeA: nodeAIPv6, nodeB: nodeBIP}
			if got := s.pins(t); !maps.Equal(got, want) {
				t.Errorf("pins = %v, want %v", got, want)
			}

			// Both serving signer names share the pin, and a renewal for
			// the pinned address changes nothing.
			s.issue(t, nodeA, servingRequest(nodeA, nodeAIPv6))
			s.issue(t, nodeA, execAgentServingRequest(nodeA, nodeAIPv6))
			if got := s.pins(t); !maps.Equal(got, want) {
				t.Errorf("pins = %v after renewals, want %v", got, want)
			}

			// A Node that is deleted keeps its pin, and one registered
			// again under its name gets no other address.
			if err := s.c.Delete(context.Background(), testNode(nodeB)); err != nil {
				t.Fatal(err)
			}
			if err := s.c.Create(context.Background(), testNode(nodeB, nodeGoneIP)); err != nil {
				t.Fatal(err)
			}
			s.wantRefused(t, s.submit(t, execAgentUser, nodeB, tc.request(nodeB, nodeGoneIP)))
			if got := s.pins(t); !maps.Equal(got, want) {
				t.Errorf("pins = %v after the Node was deleted, want %v", got, want)
			}
		})
	}
}

func TestSignerRefusesAnAddressThatIsNotTheNodesPin(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request only when its IP
	//# address is the address pinned to the requester's Node or, when no address
	//# is pinned to that Node, an address pinned to no other Node.

	t.Run("another of the Node's own addresses", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		s.wantRefused(t, s.submit(t, execAgentUser, nodeA, execAgentServingRequest(nodeA, nodeAIPv6)))
	})

	// #75: a compromised Host's kubelet lists another Host's address on its
	// own Node, and its Exec Agent asks for a certificate for it.
	t.Run("another Host's address, which its kubelet lists, while its own Node is pinned", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeB, servingRequest(nodeB, nodeBIP))
		s.setAddresses(t, nodeB, nodeBIP, nodeAIP)
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIP)))
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, execAgentServingRequest(nodeB, nodeAIP)))
	})
	t.Run("another Host's address, which its kubelet lists, pinned to that Host's Node", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		s.setAddresses(t, nodeB, nodeAIP)
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIP)))
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, execAgentServingRequest(nodeB, nodeAIP)))
		if got := s.pins(t); !maps.Equal(got, map[string]string{nodeA: nodeAIP}) {
			t.Errorf("pins = %v, want only %s's", got, nodeA)
		}
	})
	t.Run("an address pinned to another Node, whoever approved it", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		s.setAddresses(t, nodeB, nodeAIP)
		name := s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIP))
		s.approveAsAdmin(t, name)
		s.wantRefused(t, name)
		if condition(s.get(t, name), certificatesv1.CertificateFailed) == nil {
			t.Error("an approved request for another Node's address was not marked Failed")
		}
	})
	t.Run("the same address written differently", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIPv6))
		s.setPins(t, map[string]string{nodeA: "fd00:0:0:0::1"})
		s.issue(t, nodeA, execAgentServingRequest(nodeA, nodeAIPv6))
		s.setAddresses(t, nodeB, nodeBIP, nodeAIPv6)
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIPv6)))
	})
}

// TestSignerTakesANewAddressOnceAnAdministratorClearsThePin: the
// procedure 09-certificates.md#approval gives for a Host that changes
// address, and for an address DHCP moves from one Host to another.
func TestSignerTakesANewAddressOnceAnAdministratorClearsThePin(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# The Operator SHALL approve a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request only when its IP
	//# address is the address pinned to the requester's Node or, when no address
	//# is pinned to that Node, an address pinned to no other Node.

	t.Run("a Host changes address", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		s.issue(t, nodeB, servingRequest(nodeB, nodeBIP))
		s.setAddresses(t, nodeB, nodeGoneIP)
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeGoneIP)))

		s.setPins(t, map[string]string{nodeA: nodeAIP})
		s.issue(t, nodeB, servingRequest(nodeB, nodeGoneIP))
		if got, want := s.pins(t), map[string]string{nodeA: nodeAIP, nodeB: nodeGoneIP}; !maps.Equal(got, want) {
			t.Errorf("pins = %v, want %v", got, want)
		}
	})

	t.Run("DHCP moves an address to another Host", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeA, servingRequest(nodeA, nodeAIP))
		s.issue(t, nodeB, servingRequest(nodeB, nodeBIP))
		// nodeB is given nodeA's address, and nodeA a new one.
		s.setAddresses(t, nodeB, nodeAIP)
		s.setAddresses(t, nodeA, nodeGoneIP)
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIP)))

		// Clearing only nodeB's pin is not enough: the address is still
		// pinned to nodeA.
		s.setPins(t, map[string]string{nodeA: nodeAIP})
		s.wantRefused(t, s.submit(t, execAgentUser, nodeB, servingRequest(nodeB, nodeAIP)))

		// Clearing both lets each Host pin its new address.
		s.setPins(t, map[string]string{})
		s.issue(t, nodeB, servingRequest(nodeB, nodeAIP))
		s.issue(t, nodeA, servingRequest(nodeA, nodeGoneIP))
		if got, want := s.pins(t), map[string]string{nodeA: nodeGoneIP, nodeB: nodeAIP}; !maps.Equal(got, want) {
			t.Errorf("pins = %v, want %v", got, want)
		}
	})
}

func TestSignerStoresThePinBeforeTheCertificate(t *testing.T) {
	//= docs/requirements/09-certificates.md#approval
	//= type=test
	//# When the Operator signs a
	//# `battery.liquidmetal-x.dev/flintlockd-serving` or
	//# `battery.liquidmetal-x.dev/exec-agent-serving` request for a Node that has
	//# no pinned address, the Operator SHALL pin the request's IP address to that
	//# Node, and SHALL store the certificate only once the pin is stored.

	t.Run("a pin that cannot be stored leaves the request unsigned until it can", func(t *testing.T) {
		s := newSigner(t)
		refuse := true
		c := interceptor.NewClient(s.c.(client.WithWatch), interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && refuse {
					return errors.New("the API server is unavailable")
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		s.csr.Client = c
		name := s.submit(t, execAgentUser, nodeA, servingRequest(nodeA, nodeAIP))
		if _, err := s.csr.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err == nil {
			t.Fatal("the reconcile succeeded without storing the pin")
		}
		stored := s.get(t, name)
		if len(stored.Status.Certificate) != 0 {
			t.Error("the certificate was stored without its pin")
		}
		if s.pins(t) != nil {
			t.Errorf("pins = %v, want none", s.pins(t))
		}

		refuse = false
		certificate(t, s.reconcile(t, name))
		if got := s.pins(t); !maps.Equal(got, map[string]string{nodeA: nodeAIP}) {
			t.Errorf("pins = %v, want %s's", got, nodeA)
		}
	})

	t.Run("a pin checked against pins that have changed since is not stored", func(t *testing.T) {
		s := newSigner(t)
		s.issue(t, nodeB, servingRequest(nodeB, nodeBIP))
		// The check reads the pins as they were; meanwhile nodeA's address
		// is pinned to a third Node.
		stale := &corev1.ConfigMap{}
		if err := s.c.Get(context.Background(), pinsKey, stale); err != nil {
			t.Fatal(err)
		}
		s.setPins(t, map[string]string{nodeB: nodeBIP, nodeGone: nodeAIP})
		s.csr.pins = interceptor.NewClient(s.c.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && key == pinsKey {
					stale.DeepCopyInto(cm)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		name := s.submit(t, execAgentUser, nodeA, servingRequest(nodeA, nodeAIP))
		_, err := s.csr.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
		if !apierrors.IsConflict(err) {
			t.Fatalf("reconcile = %v, want a conflict on the pins", err)
		}
		if len(s.get(t, name).Status.Certificate) != 0 {
			t.Error("the certificate was stored against stale pins")
		}
		if got, want := s.pins(t), map[string]string{nodeB: nodeBIP, nodeGone: nodeAIP}; !maps.Equal(got, want) {
			t.Errorf("pins = %v, want %v", got, want)
		}

		// Against the pins as they are, the retry refuses the request.
		s.csr.pins = s.c
		s.wantRefused(t, name)
	})
}

func TestPinIndexIsBuiltOncePerVersion(t *testing.T) {
	var x pinIndex
	cm := &corev1.ConfigMap{Data: map[string]string{nodeA: nodeAIP, nodeB: nodeBIP}}
	cm.UID, cm.ResourceVersion = "u", "1"
	first := x.of(cm)
	if got := first.byAddress[nodeAIP]; len(got) != 1 || got[0] != nodeA {
		t.Errorf("byAddress[%s] = %v, want [%s]", nodeAIP, got, nodeA)
	}
	if x.of(cm.DeepCopy()) != first {
		t.Error("the index was built again for the same version")
	}
	next := cm.DeepCopy()
	next.ResourceVersion = "2"
	delete(next.Data, nodeA)
	if got := x.of(next).byAddress[nodeAIP]; len(got) != 0 {
		t.Errorf("byAddress[%s] = %v in a new version without it", nodeAIP, got)
	}
	if p := x.of(nil); p.configMap != nil || len(p.byAddress) != 0 {
		t.Error("no ConfigMap indexes to pins")
	}
}
