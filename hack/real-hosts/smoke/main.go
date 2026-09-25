//go:build realhosts

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

// Command smoke is the real hosts trial's Consumer (hack/real-hosts/README.md):
// it claims a MicroVM of a Pool with the Client Library (pkg/claimclient),
// as the Holder, runs a command in it through the Exec Agent on its Host,
// prints what the command wrote, and releases the claim.
//
// It runs on the Host, where the Exec Agent's address is reachable, with a
// kubeconfig that may request tokens for the Holder; it acts as the Holder
// with a token of the Holder's own. The build tag keeps it out of the
// module's build, tests and lint.
//
//	go build -tags realhosts ./hack/real-hosts/smoke
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"google.golang.org/grpc"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
	"github.com/phoban01/battery-operator/internal/hostcert"
	"github.com/phoban01/battery-operator/pkg/claimclient"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig of an identity that may request the Holder's tokens")
		namespace   = flag.String("namespace", "bo-trial", "namespace of the Pool, the claim and the Holder")
		pool        = flag.String("pool", "trial", "Pool to claim a MicroVM of")
		holder      = flag.String("holder", "trial-holder", "the Holder's ServiceAccount")
		operatorNS  = flag.String("operator-namespace", "battery-operator-system", "namespace of the Operator, which publishes its CAs there")
		command     = flag.String("command", "uname -a", "command to run in the MicroVM, through a shell")
		bindTimeout = flag.Duration("bind-timeout", 5*time.Minute, "how long to wait for the claim to bind")
	)
	flag.Parse()
	if err := run(*kubeconfig, *namespace, *pool, *holder, *operatorNS, *command, *bindTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
}

func run(kubeconfig, ns, pool, holder, operatorNS, command string, bindTimeout time.Duration) error {
	ctx := context.Background()
	admin, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("reading the kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := batteryv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	adminClient, err := client.New(admin, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	// The serving CA the Operator publishes, which signs every Exec Agent's
	// serving certificate (ADR 0004).
	var cm corev1.ConfigMap
	if err := adminClient.Get(ctx, client.ObjectKey{Namespace: operatorNS, Name: hostcert.CABundleConfigMap}, &cm); err != nil {
		return fmt.Errorf("reading the Operator's published CAs: %w", err)
	}
	servingCA := x509.NewCertPool()
	if !servingCA.AppendCertsFromPEM([]byte(cm.Data[hostcert.ServingCAKey])) {
		return fmt.Errorf("the ConfigMap %s/%s has no serving CA under %s", operatorNS, hostcert.CABundleConfigMap, hostcert.ServingCAKey)
	}

	// The Consumer runs as the Holder, with an ordinary token for the API
	// server; the Client Library requests the claim tokens itself.
	holderClient, err := clientAs(ctx, admin, scheme, ns, holder)
	if err != nil {
		return err
	}
	cl, err := claimclient.New(claimclient.Config{Client: holderClient, Namespace: ns, ServingCA: servingCA})
	if err != nil {
		return err
	}

	start := time.Now()
	claimCtx, cancel := context.WithTimeout(ctx, bindTimeout)
	defer cancel()
	claim, err := cl.Claim(claimCtx, claimclient.Request{
		Pool: pool, ServiceAccountName: holder, GenerateName: "smoke-",
		OnPending: func(p claimclient.Pending) { fmt.Printf("pending: %s: %s\n", p.Reason, p.Message) },
	})
	if err != nil {
		return fmt.Errorf("claiming a MicroVM of %s: %w", pool, err)
	}
	fmt.Printf("bound %s in %s: MicroVM %s on %s, Exec Agent %s\n",
		claim.Name(), time.Since(start).Round(time.Millisecond), claim.MicroVMUID(), claim.NodeName(), claim.AgentAddress())
	defer func() {
		if err := claim.Release(context.WithoutCancel(ctx)); err != nil {
			fmt.Fprintln(os.Stderr, "smoke: releasing:", err)
			return
		}
		fmt.Printf("released %s\n", claim.Name())
	}()

	conn, err := claim.Dial(ctx)
	if err != nil {
		return fmt.Errorf("dialling the Exec Agent at %s: %w", claim.AgentAddress(), err)
	}
	defer func() { _ = conn.Close() }()
	start = time.Now()
	code, err := execCommand(ctx, conn, claim.MicroVMUID(), command)
	if err != nil {
		return fmt.Errorf("running %q in MicroVM %s: %w", command, claim.MicroVMUID(), err)
	}
	fmt.Printf("%q exited %d in %s\n", command, code, time.Since(start).Round(time.Millisecond))
	if code != 0 {
		return fmt.Errorf("%q exited %d", command, code)
	}
	return nil
}

// clientAs is a client that authenticates as the ServiceAccount sa, with a
// token the admin identity requests for it.
func clientAs(ctx context.Context, admin *rest.Config, scheme *runtime.Scheme, ns, sa string) (client.Client, error) {
	cs, err := kubernetes.NewForConfig(admin)
	if err != nil {
		return nil, err
	}
	tr, err := cs.CoreV1().ServiceAccounts(ns).CreateToken(ctx, sa, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To[int64](3600)},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("requesting a token for %s/%s: %w", ns, sa, err)
	}
	rc := rest.AnonymousClientConfig(admin)
	rc.BearerToken = tr.Status.Token
	return client.New(rc, client.Options{Scheme: scheme})
}

// execCommand runs cmd through a shell in the MicroVM uid, copies its
// output to this process's, and returns its exit code.
func execCommand(ctx context.Context, conn *grpc.ClientConn, uid, cmd string) (int32, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	stream, err := execv1.NewMicroVMExecClient(conn).ExecCommand(ctx)
	if err != nil {
		return 0, err
	}
	start := &execv1.ExecCommandRequest{Payload: &execv1.ExecCommandRequest_Start{
		Start: &execv1.ExecStart{Uid: uid, Cmd: cmd, Shell: true},
	}}
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
		case *execv1.ExecCommandResponse_Stdout:
			_, _ = os.Stdout.Write(p.Stdout)
		case *execv1.ExecCommandResponse_Stderr:
			_, _ = os.Stderr.Write(p.Stderr)
		case *execv1.ExecCommandResponse_ExitCode:
			return p.ExitCode, nil
		case *execv1.ExecCommandResponse_Error:
			// The guest-agent follows an error with an exit code, which ends
			// the stream.
			fmt.Fprintf(os.Stderr, "smoke: the guest-agent reports: %s\n", p.Error)
		}
	}
}
