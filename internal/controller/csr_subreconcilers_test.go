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
	"testing"

	"github.com/go-logr/logr"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/phoban01/battery-operator/internal/reconcile"
)

// scopeOf is a csrScope for the stored request name, over c.
func (s *signer) scopeOf(t *testing.T, c client.Client, name string) *csrScope {
	t.Helper()
	return &csrScope{
		Scope:     reconcile.NewScope(s.get(t, name), c, logr.Discard(), nil),
		APIReader: s.c,
		Config:    s.csr.Config,
		cas:       s.csr.cas,
		pins:      s.c,
		pinIndex:  &s.csr.pinIndex,
	}
}

func TestCSRReviewStopsAtTheFirstFailedCheckButNotTheChain(t *testing.T) {
	s := newSigner(t)
	// Wrong requester and wrong usages: the requester check comes first.
	r := servingRequest(nodeA, nodeAIP)
	r.usages = clientUsages
	sc := s.scopeOf(t, s.c, s.submit(t, otherUser, nodeA, r))

	res, err := csrReview{}.Reconcile(context.Background(), sc)
	if err != nil || res.Stop {
		t.Fatalf("review = %+v, %v; want it to go on", res, err)
	}
	if sc.denial == nil || sc.denial.reason != ReasonRequester {
		t.Fatalf("denial = %+v, want %s", sc.denial, ReasonRequester)
	}
	if sc.req != nil {
		t.Error("the review ran the checks after the one that failed")
	}
}

func TestCSRDecisionsRecordWhatToWrite(t *testing.T) {
	s := newSigner(t)
	ctx := context.Background()

	t.Run("a failed, unapproved request is to be denied", func(t *testing.T) {
		sc := s.scopeOf(t, s.c, s.submit(t, otherUser, nodeA, servingRequest(nodeA, nodeAIP)))
		if _, err := csrChain().Run(ctx, sc); err != nil {
			t.Fatal(err)
		}
		if sc.approval == nil || sc.approval.Type != certificatesv1.CertificateDenied || sc.approval.Reason != ReasonRequester {
			t.Errorf("approval = %+v, want Denied for %s", sc.approval, ReasonRequester)
		}
		if sc.failure != nil || sc.certificate != nil {
			t.Error("a denied request is to be marked Failed or signed")
		}
		if len(sc.Object.Status.Conditions) != 0 {
			t.Error("a subreconciler changed the request's conditions before the write")
		}
	})

	t.Run("a failed, approved request is to be marked Failed", func(t *testing.T) {
		name := s.submit(t, otherUser, nodeA, servingRequest(nodeA, nodeAIP))
		s.approveAsAdmin(t, name)
		sc := s.scopeOf(t, s.c, name)
		if _, err := csrChain().Run(ctx, sc); err != nil {
			t.Fatal(err)
		}
		if sc.failure == nil || sc.failure.Reason != ReasonRequester || sc.approval != nil || sc.certificate != nil {
			t.Errorf("failure = %+v, approval = %+v; want only Failed for %s", sc.failure, sc.approval, ReasonRequester)
		}
	})

	t.Run("a request that passes is to be approved and signed", func(t *testing.T) {
		sc := s.scopeOf(t, s.c, s.submit(t, execAgentUser, nodeA, servingRequest(nodeA, nodeAIP)))
		if _, err := csrChain().Run(ctx, sc); err != nil {
			t.Fatal(err)
		}
		if sc.approval == nil || sc.approval.Type != certificatesv1.CertificateApproved || sc.certificate == nil || sc.failure != nil {
			t.Errorf("approval = %+v, certificate %d bytes, failure = %+v; want Approved and signed",
				sc.approval, len(sc.certificate), sc.failure)
		}
		if len(sc.Object.Status.Certificate) != 0 {
			t.Error("a subreconciler set the certificate before the write")
		}
		if sc.pin == nil || sc.pin.Data[nodeA] != nodeAIP {
			t.Errorf("pin = %+v, want %s pinned to %s", sc.pin, nodeA, nodeAIP)
		}
		if s.pins(t) != nil {
			t.Error("a subreconciler stored the pin before the write")
		}
	})
}

func TestCSRWriteSignsOnlyWhatIsStoredApproved(t *testing.T) {
	s := newSigner(t)
	name := s.submit(t, execAgentUser, nodeA, servingRequest(nodeA, nodeAIP))
	// The API server's answer to the approval carries Denied beside it, as
	// if someone denied the request at the same time.
	c := interceptor.NewClient(s.c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
				return err
			}
			if sub == "approval" {
				csr := obj.(*certificatesv1.CertificateSigningRequest)
				csr.Status.Conditions = append(csr.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
					Type: certificatesv1.CertificateDenied, Status: corev1.ConditionTrue, Reason: "Concurrent",
				})
			}
			return nil
		},
	})
	sc := s.scopeOf(t, c, name)
	if _, err := csrChain().Run(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	if err := sc.write(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored := s.get(t, name)
	if condition(stored, certificatesv1.CertificateApproved) == nil {
		t.Error("the approval was not stored")
	}
	if len(stored.Status.Certificate) != 0 {
		t.Error("a request not approved as stored was signed")
	}
}

func TestCSRApprovalIsStoredWhenSigningFails(t *testing.T) {
	s := newSigner(t)
	if err := s.c.Delete(context.Background(), s.serving.secret(DefaultServingCASecret)); err != nil {
		t.Fatal(err)
	}
	name := s.submit(t, execAgentUser, nodeA, servingRequest(nodeA, nodeAIP))
	if _, err := s.csr.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err == nil {
		t.Fatal("signing without the serving CA succeeded")
	}
	stored := s.get(t, name)
	if condition(stored, certificatesv1.CertificateApproved) == nil || len(stored.Status.Certificate) != 0 {
		t.Errorf("status = %+v, want approved and unsigned", stored.Status)
	}

	// Once the CA is back, the retry signs it.
	if err := s.c.Create(context.Background(), s.serving.secret(DefaultServingCASecret)); err != nil {
		t.Fatal(err)
	}
	certificate(t, s.reconcile(t, name))
}
