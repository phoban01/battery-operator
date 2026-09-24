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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/phoban01/battery-operator/internal/controller"
	"github.com/phoban01/battery-operator/internal/execagent"
	"github.com/phoban01/battery-operator/internal/hostcert"
)

// TestExecAgentAuthentication: what the Exec Agent's identity and its
// callers' tokens are to the real API server. The agent's identity, the
// token the kubelet projects into its pod, names its Host; and a caller's
// token, from TokenRequest, is reviewed with a TokenReview for the agent's
// audience, which refuses a token for the API server's own audience and one
// bound to a Secret that has since been made again. A claim token that
// authenticates is refused for want of a claim, and the agent's log names
// the Secret it read from the token. The unit tests (internal/execagent)
// cover the rest of the four checks, and the relay, against fakes.
func TestExecAgentAuthentication(t *testing.T) {
	//= docs/requirements/08-test-doubles.md#test-environments
	//= type=test
	//# The e2e suite SHALL test the behaviour that depends on the
	//# Kubernetes API server, including CRD validation, admission policies,
	//# TokenReview, TokenRequest and `CertificateSigningRequest`s, in the kind
	//# cluster.

	const (
		ns     = "e2e-exec-agent"
		holder = "holder"
		claim  = "claim"
	)
	secretName := claim + execagent.ExecSecretSuffix

	f := features.New("the Exec Agent's identity and its callers' tokens").
		Setup(createNamespace(ns)).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := mustClient(t, cfg)
			if err := c.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: holder}}); err != nil {
				t.Fatalf("creating the holder's ServiceAccount: %v", err)
			}
			if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}}); err != nil {
				t.Fatalf("creating the claim's Secret: %v", err)
			}
			return ctx
		}).
		Assess("the agent's identity names its own Host", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#identity
			//= type=test
			//# When the Exec Agent starts, the Exec Agent SHALL confirm that
			//# its identity names its own Host, and SHALL refuse to start otherwise.
			c := mustClient(t, cfg)
			hosts, err := hostNodes(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			host := hosts[0].Name
			pod, err := execAgentPod(ctx, c, host)
			if err != nil {
				t.Fatal(err)
			}
			onHost := agentClientset(ctx, t, cfg, &authenticationv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			})
			if err := execagent.CheckIdentity(ctx, onHost, host); err != nil {
				t.Errorf("the token of the agent's pod on %s: %v", host, err)
			}
			if err := execagent.CheckIdentity(ctx, onHost, otherGuardNode); err == nil {
				t.Errorf("the token of the agent's pod on %s passed as an agent on %s", host, otherGuardNode)
			}
			if err := execagent.CheckIdentity(ctx, agentClientset(ctx, t, cfg, nil), host); err == nil {
				t.Error("a token of the agent's ServiceAccount bound to no pod passed the check")
			}
			return ctx
		}).
		Assess("the agent reviews every caller's token for its own audience", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			//= docs/requirements/05-exec-agent.md#authorization
			//= type=test
			//# The Exec Agent SHALL authenticate every request with a
			//# TokenReview of the bearer token it carries, requesting the Exec Agent's
			//# audience, and SHALL refuse a request that does not authenticate.

			//= docs/requirements/05-exec-agent.md#authorization
			//= type=test
			//# The Exec Agent SHALL run a command in a MicroVM only when the
			//# request's token is bound to that claim's Secret `<claim name>-exec`, by
			//# both the Secret's name and its uid.
			c := mustClient(t, cfg)
			cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatal(err)
			}
			hosts, err := hostNodes(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			host := hosts[0].Name
			agent := dialAgent(ctx, t, c, host)
			secret, err := cs.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			token := func(audiences []string, s *corev1.Secret) string {
				t.Helper()
				tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, holder, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
					Audiences:         audiences,
					ExpirationSeconds: ptr.To[int64](3600),
					BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Secret", APIVersion: "v1", Name: s.Name, UID: s.UID},
				}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("requesting a token of %s/%s: %v", ns, holder, err)
				}
				return tr.Status.Token
			}
			agentAudience := []string{execagent.DefaultTokenAudience}

			for _, tc := range []struct {
				name, token string
			}{
				{"no token", ""},
				{"not a token", "not-a-token"},
				{"a claim token for the API server's audience", token(nil, secret)},
			} {
				if got := execStatus(ctx, t, agent, tc.token); got != codes.Unauthenticated {
					t.Errorf("%s: exec ended with %v, want %v", tc.name, got, codes.Unauthenticated)
				}
			}

			// A claim token for the agent's audience authenticates, and is
			// refused because no claim is Bound. The agent read the Secret
			// from the token, as its log says.
			if got := execStatus(ctx, t, agent, token(agentAudience, secret)); got != codes.PermissionDenied {
				t.Errorf("a claim token of no claim: exec ended with %v, want %v", got, codes.PermissionDenied)
			}
			pod, err := execAgentPod(ctx, c, host)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("bound to Secret %s/%s, but claim %s/%s does not exist", ns, secretName, ns, claim)
			var logs []byte
			err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				logs, err = cs.CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "exec-agent"}).DoRaw(ctx)
				return err == nil && strings.Contains(string(logs), want), nil
			})
			if err != nil {
				t.Errorf("the Exec Agent's log does not say %q: %v", want, err)
			}

			// A token bound to the Secret before it was made again, under
			// the same name with a new uid, no longer authenticates. It has
			// not been reviewed before, so no cached review stands in for
			// the API server's answer.
			stale := token(agentAudience, secret)
			if err := c.Delete(ctx, secret); err != nil {
				t.Fatal(err)
			}
			err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
				err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secretName}})
				return err == nil, nil
			})
			if err != nil {
				t.Fatalf("making the claim's Secret again: %v", err)
			}
			if got := execStatus(ctx, t, agent, stale); got != codes.Unauthenticated {
				t.Errorf("a token bound to an earlier Secret of the same name: exec ended with %v, want %v", got, codes.Unauthenticated)
			}
			return ctx
		}).
		Teardown(deleteNamespace(ns)).
		Feature()

	testenv.Test(t, f)
}

// agentClientset is a clientset with a token of the Exec Agent's
// ServiceAccount from a TokenRequest, bound to bound, or to nothing when
// bound is nil.
func agentClientset(ctx context.Context, t *testing.T, cfg *envconf.Config, bound *authenticationv1.BoundObjectReference) kubernetes.Interface {
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
	agent, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

// dialAgent connects to the exec API of the Exec Agent of node, at the
// address its Node report gives, verifying its certificate against the
// serving CA the Operator publishes, as a consumer does.
func dialAgent(ctx context.Context, t *testing.T, c client.Client, node string) *grpc.ClientConn {
	t.Helper()
	var n corev1.Node
	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
			return false, nil
		}
		return n.Annotations[execagent.AnnotationAddress] != "", nil
	})
	if err != nil {
		t.Fatalf("the Exec Agent of %s reports no address: %v", node, err)
	}
	var bundle corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: controller.DefaultCABundleConfigMap}, &bundle); err != nil {
		t.Fatalf("reading the CA bundle: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(bundle.Data[hostcert.ServingCAKey])) {
		t.Fatal("the CA bundle has no serving CA certificate")
	}
	conn, err := grpc.NewClient(n.Annotations[execagent.AnnotationAddress],
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// bearerToken is a static bearer token.
type bearerToken string

func (b bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(b)}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return true }

// execStatus runs a command in a MicroVM that does not exist through the
// agent, with token or none, and returns the status the exchange ended
// with. Every request of the test is refused before flintlockd is asked,
// so the MicroVM is never looked for.
func execStatus(ctx context.Context, t *testing.T, conn *grpc.ClientConn, token string) codes.Code {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var opts []grpc.CallOption
	if token != "" {
		opts = append(opts, grpc.PerRPCCredentials(bearerToken(token)))
	}
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx, opts...)
	if err != nil {
		return status.Code(err)
	}
	start := &execv1.ExecStart{Uid: "e2e-no-such-microvm", Cmd: "e2e-refused"}
	if err := stream.Send(&execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{Start: start}}); err != nil && !errors.Is(err, io.EOF) {
		return status.Code(err)
	}
	_ = stream.CloseSend()
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return codes.OK
			}
			return status.Code(err)
		}
		if _, ok := resp.GetPayload().(*execv1.ExecCommandResponse_ExitCode); ok {
			t.Error("a refused exec carried an exit code")
		}
	}
}
