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
	"fmt"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/phoban01/battery-operator/internal/execagent"
)

// admissionPolicy is the Exec Agent's ValidatingAdmissionPolicy
// (config/exec-agent/admission-policy.yaml), which the API server names
// when it refuses a request.
const admissionPolicy = "battery-operator-exec-agent-own-host"

// otherGuardNode names a Host that is not in the cluster, whose drain guard
// no Exec Agent looks after.
const otherGuardNode = "e2e-another-host"

// TestExecAgentAdmissionPolicy: the Exec Agent's identity, a token bound to
// its pod on one Host, may change only that Host's Node's annotations under
// battery's prefix, and create and delete only that Host's drain guard.
// The token is the one the kubelet would project: a TokenRequest bound to
// the agent's pod, which carries the pod's node in the identity.
func TestExecAgentAdmissionPolicy(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.

	// Filled in by the setup: the Host whose agent the test speaks as, a
	// Node of another Host and one that is no Host, and clients with the
	// agent's identity, with and without its node.
	var (
		host, otherHost, otherGuardHost, notHost string
		asAgent, asAgentNoNode                   client.Client
	)

	f := features.New("the Exec Agent's admission policy").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			hosts, err := hostNodes(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			// kind-config.yaml has two Hosts and a control plane that is
			// none. A smaller cluster (KIND_CONFIG) skips the cases that
			// need those Nodes; a guard for another Host needs only a name.
			host, otherHost, otherGuardHost = hosts[0].Name, "", otherGuardNode
			if len(hosts) > 1 {
				otherHost, otherGuardHost = hosts[1].Name, hosts[1].Name
			}
			if n, err := nonHostNode(ctx, c); err == nil {
				notHost = n.Name
			}

			pod, err := execAgentPod(ctx, c, host)
			if err != nil {
				t.Fatal(err)
			}
			asAgent = agentClient(ctx, t, cfg, &authenticationv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			})
			asAgentNoNode = agentClient(ctx, t, cfg, nil)
			return ctx
		}).
		Assess("the agent may annotate its own Host's Node under battery's prefix", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#identity
			//= type=test
			//# The Manifests SHALL include a ValidatingAdmissionPolicy that
			//# lets an Exec Agent's identity change only the annotations of its own
			//# Host's Node under the prefix `battery.liquidmetal-x.dev/`, and nothing
			//# else of any Node.
			c := mustClient(t, cfg)
			if err := patchNode(ctx, c, asAgent, host, func(n *corev1.Node) {
				setAnnotation(n, execagent.Prefix+"e2e", "allowed")
			}); err != nil {
				t.Errorf("annotating its own Node under the prefix: %v", err)
			}
			// The report it keeps there, which the Exec Agent of the Host has
			// written by now, it may also remove.
			var n corev1.Node
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
				if err := c.Get(ctx, client.ObjectKey{Name: host}, &n); err != nil {
					return false, nil
				}
				_, ok := n.Annotations[execagent.AnnotationReady]
				return ok, nil
			})
			if err != nil {
				t.Fatalf("the Exec Agent of %s wrote no report: %v", host, err)
			}
			if err := patchNode(ctx, c, asAgent, host, func(n *corev1.Node) {
				delete(n.Annotations, execagent.AnnotationReady)
			}); err != nil {
				t.Errorf("removing a report annotation of its own Node: %v", err)
			}
			return ctx
		}).
		Assess("the agent may change nothing else of any Node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#identity
			//= type=test
			//# The Manifests SHALL include a ValidatingAdmissionPolicy that
			//# lets an Exec Agent's identity change only the annotations of its own
			//# Host's Node under the prefix `battery.liquidmetal-x.dev/`, and nothing
			//# else of any Node.
			c := mustClient(t, cfg)
			for _, tc := range []struct {
				name   string
				client client.Client
				node   string
				mutate func(*corev1.Node)
			}{
				{"another annotation of its own Node", asAgent, host, func(n *corev1.Node) {
					setAnnotation(n, "example.com/e2e", "refused")
				}},
				{"a label of its own Node", asAgent, host, func(n *corev1.Node) {
					n.Labels["example.com/e2e"] = "refused"
				}},
				{"its own Node's spec", asAgent, host, func(n *corev1.Node) {
					n.Spec.Unschedulable = !n.Spec.Unschedulable
				}},
				{"another Host's Node", asAgent, otherHost, func(n *corev1.Node) {
					setAnnotation(n, execagent.Prefix+"e2e", "refused")
				}},
				{"a Node that is no Host", asAgent, notHost, func(n *corev1.Node) {
					setAnnotation(n, execagent.Prefix+"e2e", "refused")
				}},
				{"its own Node, by an identity that names no node", asAgentNoNode, host, func(n *corev1.Node) {
					setAnnotation(n, execagent.Prefix+"e2e", "refused")
				}},
				{"another prefix's annotation of its own Node, removed", asAgent, host, func(n *corev1.Node) {
					for key := range n.Annotations {
						if !strings.HasPrefix(key, execagent.Prefix) {
							delete(n.Annotations, key)
							return
						}
					}
				}},
				{"a look-alike of the prefix on its own Node", asAgent, host, func(n *corev1.Node) {
					setAnnotation(n, "evil"+execagent.Prefix+"e2e", "refused")
				}},
				{"a subdomain of the prefix on its own Node", asAgent, host, func(n *corev1.Node) {
					setAnnotation(n, "host-service."+execagent.Prefix+"e2e", "refused")
				}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.node == "" {
						t.Skip("the cluster has no such Node")
					}
					wantRefused(t, patchNode(ctx, c, tc.client, tc.node, tc.mutate))
				})
			}
			// Nor its own Node's status, which RBAC refuses it before the
			// policy is asked.
			t.Run("its own Node's status", func(t *testing.T) {
				var n corev1.Node
				if err := c.Get(ctx, client.ObjectKey{Name: host}, &n); err != nil {
					t.Fatal(err)
				}
				changed := n.DeepCopy()
				changed.Status.Conditions = append(changed.Status.Conditions, corev1.NodeCondition{Type: "E2E", Status: corev1.ConditionFalse})
				err := asAgent.Status().Patch(ctx, changed, client.MergeFrom(&n), client.DryRunAll)
				if !apierrors.IsForbidden(err) {
					t.Errorf("patching its own Node's status: got %v, want it refused", err)
				}
			})
			return ctx
		}).
		Assess("the agent may create and delete its own Host's drain guard", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#identity
			//= type=test
			//# The ValidatingAdmissionPolicy of EA-051 SHALL let an Exec
			//# Agent's identity create and delete guard pods and PodDisruptionBudgets
			//# only for its own Host's Node.
			budget := guardBudget(execagent.GuardName(host))
			if err := asAgent.Create(ctx, budget); err != nil {
				t.Fatalf("creating its guard budget: %v", err)
			}
			if err := asAgent.Delete(ctx, budget); err != nil {
				t.Errorf("deleting its guard budget: %v", err)
			}
			pod := guardPod(execagent.GuardName(host), host)
			if err := asAgent.Create(ctx, pod); err != nil {
				t.Fatalf("creating its guard pod: %v", err)
			}
			if err := asAgent.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Errorf("deleting its guard pod: %v", err)
			}
			return ctx
		}).
		Assess("the agent may make no other Host's drain guard", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#identity
			//= type=test
			//# The ValidatingAdmissionPolicy of EA-051 SHALL let an Exec
			//# Agent's identity create and delete guard pods and PodDisruptionBudgets
			//# only for its own Host's Node.
			for _, tc := range []struct {
				name string
				obj  client.Object
			}{
				{"its own guard pod on another Host", guardPod(execagent.GuardName(host), otherGuardHost)},
				{"another Host's guard pod", guardPod(execagent.GuardName(otherGuardHost), otherGuardHost)},
				{"another Host's guard budget", guardBudget(execagent.GuardName(otherGuardHost))},
				{"a guard pod with a ServiceAccount token", func() client.Object {
					p := guardPod(execagent.GuardName(host), host)
					p.Spec.AutomountServiceAccountToken = nil
					return p
				}()},
				{"a pod of another name", guardPod("not-the-guard", host)},
				{"a budget of another name", guardBudget("not-the-guard")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					wantRefused(t, asAgent.Create(ctx, tc.obj, client.DryRunAll))
				})
			}
			// And no other Host's guard may be deleted: one made by the
			// cluster's administrator stands in for it.
			c := mustClient(t, cfg)
			budget := guardBudget(execagent.GuardName(otherGuardHost))
			if err := c.Create(ctx, budget); err != nil {
				t.Fatalf("creating another Host's guard budget: %v", err)
			}
			defer func() { _ = c.Delete(ctx, budget) }()
			wantRefused(t, asAgent.Delete(ctx, guardBudget(execagent.GuardName(otherGuardHost))))
			// Nor its guard pod: a guard pod for a Node that does not exist
			// stands in for another Host's, since no Exec Agent removes it
			// while the test runs.
			pod := guardPod(execagent.GuardName(otherGuardNode), otherGuardNode)
			if err := c.Create(ctx, pod); err != nil {
				t.Fatalf("creating another Host's guard pod: %v", err)
			}
			defer func() { _ = c.Delete(ctx, pod, client.GracePeriodSeconds(0)) }()
			wantRefused(t, asAgent.Delete(ctx, guardPod(execagent.GuardName(otherGuardNode), otherGuardNode), client.DryRunAll))
			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

// execAgentPod is the Exec Agent's pod on node.
func execAgentPod(ctx context.Context, c client.Client, node string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/component": "exec-agent"}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if p := &pods.Items[i]; p.Spec.NodeName == node && p.DeletionTimestamp == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no Exec Agent pod runs on %s", node)
}

// agentClient is a client with a token of the Exec Agent's ServiceAccount
// from a TokenRequest, bound to bound, or to nothing when bound is nil.
func agentClient(ctx context.Context, t *testing.T, cfg *envconf.Config, bound *authenticationv1.BoundObjectReference) client.Client {
	t.Helper()
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := cs.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, execAgentServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{BoundObjectRef: bound, ExpirationSeconds: ptr.To[int64](3600)},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("requesting a token for the Exec Agent: %v", err)
	}
	rc := rest.AnonymousClientConfig(cfg.Client().RESTConfig())
	rc.BearerToken = tr.Status.Token
	c, err := client.New(rc, client.Options{Scheme: mustClient(t, cfg).Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// patchNode reads node with c, and patches it with as, changed by mutate,
// in a dry run: the admission policy decides a dry run as it would the
// request, and the Node stays as it is.
func patchNode(ctx context.Context, c, as client.Client, node string, mutate func(*corev1.Node)) error {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return err
	}
	changed := n.DeepCopy()
	if changed.Labels == nil {
		changed.Labels = map[string]string{}
	}
	mutate(changed)
	return as.Patch(ctx, changed, client.MergeFrom(&n), client.DryRunAll)
}

func setAnnotation(n *corev1.Node, key, value string) {
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[key] = value
}

// guardPod is a drain guard pod as the Exec Agent makes it.
func guardPod(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			AutomountServiceAccountToken: ptr.To(false),
			Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:                   []corev1.Container{{Name: "guard", Image: execagent.DefaultGuardImage}},
		},
	}
}

// guardBudget is a drain guard's PodDisruptionBudget.
func guardBudget(name string) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"e2e": name}},
		},
	}
}

// wantRefused fails t unless the Exec Agent's admission policy refused
// the request that returned err; RBAC's refusal does not count.
func wantRefused(t *testing.T, err error) {
	t.Helper()
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		t.Fatalf("got error %v, want the admission policy's refusal", err)
	}
	if !strings.Contains(err.Error(), admissionPolicy) {
		t.Fatalf("got error %q, want the refusal of %s", err, admissionPolicy)
	}
}
