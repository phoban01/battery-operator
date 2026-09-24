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

package inventory

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestRenewRecordsTheCertificateBatteryStartedWith checks that with no
// client certificate recorded, the Inventory Controller records the one
// the Secret holds, which battery read when its pod started, and does not
// restart battery for it; and that with no Secret it records nothing.
func TestRenewRecordsTheCertificateBatteryStartedWith(t *testing.T) {
	h := newHarness(t)
	h.cert = nil
	h.mustReconcile()
	if got := h.stored().ClientCertificate; got != "" {
		t.Errorf("with no Secret, recorded %q", got)
	}

	h.cert = certificate1
	r := h.mustReconcile()
	if got := h.stored().ClientCertificate; got != Digest(certificate1) {
		t.Errorf("recorded %q, want the Secret's certificate %q", got, Digest(certificate1))
	}
	if _, open := h.state.WindowOpened(); open || r.RequeueAfter != 0 {
		t.Errorf("recording the certificate opened a window (requeue %s)", r.RequeueAfter)
	}
	h.advance(10 * window)
	if n := h.restarter.count(); n != 0 {
		t.Errorf("restarts = %d, want none", n)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# When the client certificate in the Secret of DP-005 changes,
//# the Operator SHALL restart battery through the mechanism of DP-006, and
//# SHALL signal battery only once the Operator's own mount of that Secret
//# holds the new certificate.

// TestRenewedCertificateRestartsBattery covers DP-007 from the Inventory
// Controller's side: a renewed certificate restarts battery through the
// one Restarter, with its configuration unchanged and the renewed
// certificate to wait for, and battery is then left alone.
func TestRenewedCertificateRestartsBattery(t *testing.T) {
	h := newHarness(t)
	h.mustReconcile()
	before := h.stored()

	h.cert = certificate2
	if r := h.mustReconcile(); r.RequeueAfter != window {
		t.Errorf("RequeueAfter = %s, want the window %s", r.RequeueAfter, window)
	}
	if n := h.restarter.count(); n != 0 {
		t.Fatalf("restarts = %d before the window closed", n)
	}
	h.advance(window)
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	if !bytes.Equal(h.restarter.certs[0], certificate2) {
		t.Errorf("battery restarted waiting for %q, want the renewed certificate", h.restarter.certs[0])
	}
	if h.pools.lists != 0 {
		t.Errorf("a restart for a renewal alone listed battery's Pools %d times; it removes no Host, so Drain has nothing to wait for", h.pools.lists)
	}
	if !bytes.Equal(h.restarter.configs[0], before.Raw) {
		t.Error("a renewal changed battery's configuration")
	}
	after := h.stored()
	if after.Pending || after.ClientCertificate != Digest(certificate2) {
		t.Errorf("after the restart: pending %t, certificate %q, want the renewed one recorded", after.Pending,
			after.ClientCertificate)
	}

	h.advance(10 * window)
	if n := h.restarter.count(); n != 1 {
		t.Errorf("restarts = %d with nothing renewed since, want 1", n)
	}
}

//= docs/requirements/06-deployment.md#battery-sidecar
//= type=test
//# The Operator SHALL restart battery for a changed client
//# certificate at the close of a restart window of IN-012, in the same
//# single restart as every change to battery's Hosts that has settled by
//# then.

// TestRenewalAndHostChangeRestartOnce covers DP-008: a renewal that comes
// while a window is open for a Host change, or a Host change that settles
// while a window is open for a renewal, goes into the same restart; and a
// renewal just after a restart waits a whole window.
func TestRenewalAndHostChangeRestartOnce(t *testing.T) {
	t.Run("renewal in a Host change's window", func(t *testing.T) {
		h := newHarness(t, host(nodeA, addrA))
		h.mustReconcile()
		h.advance(settle) // node-a settles: the window opens
		h.clock.Advance(window / 2)
		h.cert = certificate2
		h.mustReconcile()
		h.advance(window / 2) // the window closes
		assertOneRestart(t, h, Hosts{nodeA: addrA}, certificate2)
	})
	t.Run("Host change in a renewal's window", func(t *testing.T) {
		h := newHarness(t)
		h.mustReconcile()
		h.cert = certificate2
		h.mustReconcile() // the window opens
		h.create(host(nodeA, addrA))
		h.mustReconcile()
		h.advance(settle) // node-a settles within the window
		h.advance(window - settle)
		assertOneRestart(t, h, Hosts{nodeA: addrA}, certificate2)
	})
	t.Run("renewal just after a restart", func(t *testing.T) {
		h := newHarness(t, host(nodeA, addrA))
		joinAll(h)
		h.cert = certificate2
		h.mustReconcile()
		h.advance(window)
		if n := h.restarter.count(); n != 2 {
			t.Fatalf("restarts = %d, want 2", n)
		}
		if gap := h.restarter.at[1].Sub(h.restarter.at[0]); gap < window {
			t.Errorf("restarts %s apart, want at least the window %s", gap, window)
		}
	})
}

// assertOneRestart checks that battery restarted once, with hosts and cert.
func assertOneRestart(t *testing.T, h *harness, hosts Hosts, cert []byte) {
	t.Helper()
	if n := h.restarter.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}
	if got := hostsOf(h.lastRestart()); !got.Equal(hosts) {
		t.Errorf("battery restarted with %v, want %v", got, hosts)
	}
	if !bytes.Equal(h.restarter.certs[0], cert) {
		t.Errorf("battery restarted waiting for %q, want %q", h.restarter.certs[0], cert)
	}
	if got := h.stored().ClientCertificate; got != Digest(cert) {
		t.Errorf("recorded %q, want %q", got, Digest(cert))
	}
}

// TestResumeRestartsWithTheCurrentCertificate checks that a restart for a
// renewal that failed is finished with the certificate the Secret holds
// by then, which is what gets recorded, and that battery is not restarted
// again for it.
func TestResumeRestartsWithTheCurrentCertificate(t *testing.T) {
	h := newHarness(t)
	h.mustReconcile()
	h.cert = certificate2
	h.mustReconcile()
	h.clock.Advance(window)
	h.restarter.err = errors.New("the mount did not change")
	if _, err := h.reconcile(); err == nil {
		t.Fatal("a failed restart did not fail the reconcile")
	}
	if got := h.stored(); !got.Pending || got.ClientCertificate != Digest(certificate1) {
		t.Fatalf("after the failed restart: pending %t, certificate %q, want pending with the old one",
			got.Pending, got.ClientCertificate)
	}

	// Renewed again meanwhile.
	certificate3 := []byte("-----BEGIN CERTIFICATE-----\nthree\n-----END CERTIFICATE-----\n")
	h.cert = certificate3
	h.restarter.err = nil
	h.advance(time.Second)
	if n := h.restarter.count(); n != 1 || !bytes.Equal(h.restarter.certs[0], certificate3) {
		t.Fatalf("restarts %d, waiting for %q; want one, for the certificate the Secret holds now", n, h.restarter.certs)
	}
	if got := h.stored(); got.Pending || got.ClientCertificate != Digest(certificate3) {
		t.Errorf("after the resumed restart: pending %t, certificate %q", got.Pending, got.ClientCertificate)
	}
	h.advance(10 * window)
	if n := h.restarter.count(); n != 1 {
		t.Errorf("restarts = %d, want 1", n)
	}
}
