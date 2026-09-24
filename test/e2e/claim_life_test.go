//go:build e2e

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

package e2e

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/pkg/claimclient"
)

// claimLifeKey carries the claim, and the connection to its Exec Agent,
// from one assessment of the claim's life to the next.
type claimLifeKey struct{}

// claimLife is what the assessments of TestClientLibraryClaimLife share.
type claimLife struct {
	claim *claimclient.Claim
	conn  *grpc.ClientConn
	ns    string
}

// TestClientLibraryClaimLife: a Consumer, as the Holder, takes a claim
// through its whole life with the Client Library (pkg/claimclient): it
// claims a MicroVM of a Ready Pool, runs a command in it through the Exec
// Agent on its Host, renews the claim, and releases it, and then the
// claim's Secret, and with it the claim token, is gone.
//
// The test process is the Consumer, so the Exec Agent has to be reachable
// from it: the claim's agent address is the Host's internal address, where
// the agent serves in the node's network namespace, and a kind node's
// internal address is on the Docker network that the machine running kind
// reaches directly (test/e2e/README.md, "Reaching the Exec Agent"). No
// test pod runs the Client Library in the cluster.
func TestClientLibraryClaimLife(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL take a Client Library claim through its
	//# whole life: bind, run a command through the Exec Agent, renew, and
	//# release.
	ns := envconf.RandomName("e2e-claim-life", 20)
	testenv.Test(t, claimLifeFeature(ns))
}

func claimLifeFeature(ns string) features.Feature {
	const poolName = "life"
	return features.New("a Client Library claim through its whole life").
		Setup(createNamespace(ns)).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			for _, obj := range append(holderObjects(ns), placedPool(ns, poolName, map[string]string{hostLabel: isTrue})) {
				if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
					t.Fatalf("creating %T %s: %v", obj, obj.GetName(), err)
				}
			}
			waitForPoolReady(ctx, t, c, placedPool(ns, poolName, nil), metav1.ConditionTrue, poolReasonAtSize, nil)
			return ctx
		}).
		Assess("the Holder claims a MicroVM, and gets it once the claim is Bound", claimWithLibrary(ns, poolName)).
		Assess("a command runs in the MicroVM through the Exec Agent, with the claim token", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/07-client.md#using
			//= type=test
			//# The Client Library SHALL connect to the Exec Agent at the
			//# address in the claim's status, verifying its serving certificate against
			//# the certificate authority the Consumer configures, and SHALL send the
			//# current claim token as the bearer token of every request.

			//= docs/requirements/05-exec-agent.md#authorization
			//= type=test
			//# The Exec Agent SHALL authenticate every request with a
			//# TokenReview of the bearer token it carries, requesting the Exec Agent's
			//# audience, and SHALL refuse a request that does not authenticate.
			life := lifeOf(ctx, t)
			conn, err := life.claim.Dial(ctx)
			if err != nil {
				t.Fatalf("dialling the Exec Agent at %s: %v", life.claim.AgentAddress(), err)
			}
			life.conn = conn
			code, err := execCommand(ctx, conn, life.claim.MicroVMUID(), "true")
			if err != nil {
				t.Fatalf("running a command in MicroVM %s through the Exec Agent at %s: %v",
					life.claim.MicroVMUID(), life.claim.AgentAddress(), err)
			}
			if code != 0 {
				t.Fatalf("the command exited %d, want 0", code)
			}
			return ctx
		}).
		Assess("the Client Library renews the claim, and the Operator relays it to battery", renewalsRelayed).
		Assess("the API server refuses a renewal of the claim under another uid", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			// The Client Library puts the claim's uid in every renewal, so
			// that it never renews a claim made again under the same name.
			life := lifeOf(ctx, t)
			holder := holderClient(ctx, t, cfg, life.ns)
			patch, err := json.Marshal(map[string]any{
				"metadata": map[string]any{"uid": "00000000-0000-0000-0000-000000000000"},
				"spec":     map[string]any{"renewTime": metav1.NowMicro()},
			})
			if err != nil {
				t.Fatal(err)
			}
			obj := &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{
				Namespace: life.ns, Name: life.claim.Name(),
			}}
			err = holder.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch))
			if !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
				t.Fatalf("a renewal under another uid: got %v, want it refused as a conflict", err)
			}
			return ctx
		}).
		Assess("releasing deletes the claim, and its Secret and token go with it", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/07-client.md#using
			//= type=test
			//# When the Consumer releases a claim, the Client Library SHALL
			//# delete the claim.
			life := lifeOf(ctx, t)
			c := mustClient(t, cfg)
			ns := life.ns
			if err := life.claim.Release(ctx); err != nil {
				t.Fatalf("releasing the claim %s: %v", life.claim.Name(), err)
			}
			waitForGone(ctx, t, c, &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: life.claim.Name()}})
			// The garbage collector deletes the Secret, whose owner the
			// claim was.
			waitForGone(ctx, t, c, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: ns, Name: batteryv1alpha1.ExecSecretName(life.claim.Name()),
			}})

			//= docs/requirements/05-exec-agent.md#authorization
			//= type=test
			//# The Exec Agent SHALL run a command in a MicroVM only when that
			//# claim is Bound, its Lease has not expired, and it names that MicroVM's uid
			//# and this Host.
			//
			// The connection still sends the claim's last token, which is
			// bound to a Secret that is gone.
			_, err := execCommand(ctx, life.conn, life.claim.MicroVMUID(), "true")
			if code := status.Code(err); code != codes.Unauthenticated && code != codes.PermissionDenied {
				t.Fatalf("a command after the claim is released: got %v, want it refused", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if life, ok := ctx.Value(claimLifeKey{}).(*claimLife); ok {
				if life.conn != nil {
					_ = life.conn.Close()
				}
				_ = life.claim.Release(context.WithoutCancel(ctx))
			}
			return ctx
		}).
		Teardown(deleteNamespace(ns)).
		Feature()
}

// claimWithLibrary claims a MicroVM of pool with the Client Library, as
// the Holder, and leaves the claim in the context.
func claimWithLibrary(ns, pool string) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		//= docs/requirements/07-client.md#claiming
		//= type=test
		//# The Client Library SHALL create a claim with the Pool and
		//# Holder the Consumer names, and then an empty Secret `<claim name>-exec`
		//# whose owner reference is the claim.

		//= docs/requirements/07-client.md#claiming
		//= type=test
		//# The Client Library SHALL return a claim to the Consumer only once
		//# the claim's phase is `Bound`, together with the MicroVM's uid, the
		//# Host's node name and the Exec Agent's address from the claim's status.
		cl := consumerLibrary(ctx, t, cfg, ns)
		claimCtx, cancel := context.WithTimeout(ctx, placementTimeout)
		defer cancel()
		claim, err := cl.Claim(claimCtx, claimclient.Request{Pool: pool, ServiceAccountName: holderName, GenerateName: "life-"})
		if err != nil {
			t.Fatalf("claiming a MicroVM of %s: %v", pool, err)
		}
		c := mustClient(t, cfg)
		obj := &batteryv1alpha1.MicroVMClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: claim.Name()}, obj); err != nil {
			t.Fatal(err)
		}
		st := obj.Status
		if st.Phase != batteryv1alpha1.MicroVMClaimBound || st.MicroVM == nil || st.Host == nil {
			t.Fatalf("the claim returned is not Bound: %+v", st)
		}
		if claim.MicroVMUID() != st.MicroVM.UID || claim.NodeName() != st.Host.NodeName ||
			claim.AgentAddress() != st.Host.AgentAddress || claim.AgentAddress() == "" {
			t.Fatalf("the claim returned names MicroVM %q on %q at %q, the claim's status %+v",
				claim.MicroVMUID(), claim.NodeName(), claim.AgentAddress(), st)
		}
		secret := &corev1.Secret{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: batteryv1alpha1.ExecSecretName(claim.Name())}, secret); err != nil {
			t.Fatalf("reading the claim's Secret: %v", err)
		}
		if refs := secret.OwnerReferences; len(refs) != 1 || refs[0].UID != obj.UID || len(secret.Data) != 0 {
			t.Fatalf("the claim's Secret has owners %+v and %d keys, want the claim alone and none", refs, len(secret.Data))
		}
		return context.WithValue(ctx, claimLifeKey{}, &claimLife{claim: claim, ns: ns})
	}
}

// renewalsRelayed waits for the Operator to relay two renewals of the
// claim that the Client Library holds.
func renewalsRelayed(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	//= docs/requirements/07-client.md#holding
	//= type=test
	//# While the Consumer holds a Bound claim, the Client Library SHALL
	//# set the claim's `spec.renewTime` at the Pool's heartbeat interval.

	//= docs/requirements/02-claims.md#renewal
	//= type=test
	//# While the `spec.renewTime` of a Bound claim differs from the
	//# last `renewTime` the Claim Controller relayed for it, the Claim
	//# Controller SHALL call battery's `Heartbeat` for the claim's Lease, and SHALL
	//# write the expiry time battery returns and the `renewTime` it relayed to the
	//# claim's status in one write.
	life := lifeOf(ctx, t)
	c := mustClient(t, cfg)
	key := client.ObjectKey{Namespace: life.ns, Name: life.claim.Name()}
	// Two relayed renewals, each later than the last: the Client
	// Library renews at the Pool's heartbeat interval, two seconds,
	// and the Operator relays each renewal.
	var relayed []metav1.MicroTime
	obj := &batteryv1alpha1.MicroVMClaim{}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		st := obj.Status
		if st.ObservedRenewTime == nil || st.LeaseExpiresAt == nil || st.Phase != batteryv1alpha1.MicroVMClaimBound {
			return false, nil
		}
		if n := len(relayed); n == 0 || relayed[n-1].Before(st.ObservedRenewTime) {
			relayed = append(relayed, *st.ObservedRenewTime)
		}
		return len(relayed) >= 2, nil
	})
	if err != nil {
		t.Fatalf("waiting for two relayed renewals of %s: %v; saw %v, and its status is %s",
			key.Name, err, relayed, describe(obj.Status))
	}
	if life.claim.Err() != nil {
		t.Fatalf("the Lease is lost while the claim is held: %v", life.claim.Err())
	}
	return ctx
}

// holderObjects are the Holder, a ServiceAccount, and a Role that grants
// it what the Client Library needs (pkg/claimclient's documentation).
func holderObjects(ns string) []client.Object {
	const create, get = "create", "get"
	battery := batteryv1alpha1.GroupVersion.Group
	return []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holderName}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holderName},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{battery}, Resources: []string{"microvmclaims"}, Verbs: []string{create, get, "patch", "delete"}},
				{APIGroups: []string{battery}, Resources: []string{"pools"}, Verbs: []string{get}},
				{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{create}},
				{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, Verbs: []string{create}, ResourceNames: []string{holderName}},
			},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holderName},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: holderName},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: holderName}},
		},
	}
}

// holderClient is a client that runs as the Holder, with an ordinary token
// for the API server.
func holderClient(ctx context.Context, t *testing.T, cfg *envconf.Config, ns string) client.Client {
	t.Helper()
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, holderName, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To[int64](3600)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token for the Holder: %v", err)
	}
	rc := rest.AnonymousClientConfig(cfg.Client().RESTConfig())
	rc.BearerToken = tr.Status.Token
	c, err := client.New(rc, client.Options{Scheme: mustClient(t, cfg).Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// consumerLibrary is the Client Library, running as the Holder, with the
// serving CA the Operator publishes.
func consumerLibrary(ctx context.Context, t *testing.T, cfg *envconf.Config, ns string) *claimclient.Client {
	t.Helper()
	//= docs/requirements/08-test-doubles.md#test-environments
	//# The e2e suite SHALL take a Client Library claim through its
	//# whole life: bind, run a command through the Exec Agent, renew, and
	//# release.
	cl, err := claimclient.New(claimclient.Config{
		Client:    holderClient(ctx, t, cfg, ns),
		Namespace: ns,
		ServingCA: servingCA(ctx, t, mustClient(t, cfg)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// servingCA is the serving CA the Operator publishes, which signs every
// Exec Agent's serving certificate.
func servingCA(ctx context.Context, t *testing.T, c client.Client) *x509.CertPool {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: hostcert.CABundleConfigMap}, &cm); err != nil {
		t.Fatalf("reading the published CAs: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cm.Data[hostcert.ServingCAKey])) {
		t.Fatalf("the ConfigMap %s has no serving CA under %s", hostcert.CABundleConfigMap, hostcert.ServingCAKey)
	}
	return pool
}

// execCommand runs cmd in the MicroVM uid through conn, and returns its
// exit code.
func execCommand(ctx context.Context, conn *grpc.ClientConn, uid, cmd string) (int32, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		return 0, err
	}
	start := &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: &execv1.ExecStart{Uid: uid, Cmd: cmd}}}
	if err := stream.Send(start); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if err := stream.CloseSend(); err != nil {
		return 0, err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return 0, err
		}
		switch p := resp.GetPayload().(type) {
		case *execv1.ExecCommandResponse_ExitCode:
			return p.ExitCode, nil
		case *execv1.ExecCommandResponse_Error:
			return 0, fmt.Errorf("the command failed: %s", p.Error)
		}
	}
}

// lifeOf is the claim an earlier assessment made, failing t when there is
// none.
func lifeOf(ctx context.Context, t *testing.T) *claimLife {
	t.Helper()
	life, ok := ctx.Value(claimLifeKey{}).(*claimLife)
	if !ok {
		t.Fatal("no claim was made")
	}
	return life
}
