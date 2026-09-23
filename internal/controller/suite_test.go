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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

const (
	// operatorNamespace is the Operator's namespace in the tests.
	operatorNamespace = "battery-operator-system"
	// operatorUser is the Operator's identity: the manager runs as this
	// user, with the RBAC controller-gen generates from the markers.
	operatorUser = "system:serviceaccount:" + operatorNamespace + ":controller-manager"
	// execAgentUser is the Exec Agent's ServiceAccount.
	execAgentUser = "system:serviceaccount:" + operatorNamespace + ":exec-agent"
	// trustDomain is the SPIFFE trust domain the Operator is configured
	// with.
	trustDomain = "example.test"
	// maxDuration is the configured maximum certificate duration.
	maxDuration = 24 * time.Hour
	// leaderElectionID names the manager's Lease.
	leaderElectionID = "battery-operator-test"
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client

	// servingCA expires before maxDuration has passed, so that the
	// certificates it signs end with it. clientCA outlives maxDuration.
	servingCA *testCA
	clientCA  *testCA
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = certificatesv1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: false,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	By("creating the Operator's namespace, its RBAC and the CA Secrets")
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace}})).To(Succeed())
	applyOperatorRBAC("role.yaml", "leader_election_role.yaml")
	allowCreatingCSRs()
	servingCA = newTestCA("serving CA", time.Now().Add(2*time.Hour))
	clientCA = newTestCA("flintlockd client CA", time.Now().Add(10*365*24*time.Hour))
	Expect(k8sClient.Create(ctx, servingCA.secret(DefaultServingCASecret))).To(Succeed())
	Expect(k8sClient.Create(ctx, clientCA.secret(DefaultClientCASecret))).To(Succeed())

	By("starting the manager as the Operator's identity")
	operatorCfg := rest.CopyConfig(cfg)
	operatorCfg.Impersonate = rest.ImpersonationConfig{UserName: operatorUser}
	mgr, err := ctrl.NewManager(operatorCfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Leader election runs as in the Manifests, with the leader-election
		// Role: the controllers start only once the manager holds the Lease.
		LeaderElection:          true,
		LeaderElectionNamespace: operatorNamespace,
		LeaderElectionID:        leaderElectionID,
	})
	Expect(err).NotTo(HaveOccurred())
	signerConfig := SignerConfig{
		TrustDomain:             trustDomain,
		MaxDuration:             maxDuration,
		Namespace:               operatorNamespace,
		ServingCASecret:         DefaultServingCASecret,
		ClientCASecret:          DefaultClientCASecret,
		CABundleConfigMap:       DefaultCABundleConfigMap,
		ExecAgentServiceAccount: "exec-agent",
	}
	Expect(signerConfig.Complete()).To(Succeed())
	Expect((&CertificateSigningRequestReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Config:    signerConfig,
	}).SetupWithManager(mgr)).To(Succeed())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	Eventually(func() error {
		return testEnv.Stop()
	}, time.Minute, time.Second).Should(Succeed())
})

// applyOperatorRBAC creates the ClusterRoles and Roles in the named files of
// config/rbac, and binds each to the Operator's identity, so that the manager
// runs with exactly that RBAC: role.yaml, which controller-gen generates from
// the markers, and the leader-election Role.
func applyOperatorRBAC(files ...string) {
	subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", file))
		Expect(err).NotTo(HaveOccurred())
		for doc := range bytes.SplitSeq(data, []byte("\n---\n")) {
			u := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(doc, &u.Object)).To(Succeed())
			if len(u.Object) == 0 {
				continue
			}
			ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: u.GetKind(), Name: u.GetName()}
			switch u.GetKind() {
			case "ClusterRole":
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
				Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{Name: u.GetName()},
					RoleRef:    ref,
					Subjects:   subjects,
				})).To(Succeed())
			case "Role":
				u.SetNamespace(operatorNamespace)
				Expect(k8sClient.Create(ctx, u)).To(Succeed())
				Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{Namespace: operatorNamespace, Name: u.GetName()},
					RoleRef:    ref,
					Subjects:   subjects,
				})).To(Succeed())
			default:
				Fail("unexpected kind in " + file + ": " + u.GetKind())
			}
		}
	}
}

// csrRequester names the ClusterRole and binding of allowCreatingCSRs.
const csrRequester = "csr-requester"

// allowCreatingCSRs lets every authenticated user create and read
// CertificateSigningRequests, as the Manifests let the Exec Agent (EA-067),
// so that the tests can create them as any requester.
func allowCreatingCSRs() {
	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: csrRequester},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{certificatesv1.GroupName},
			Resources: []string{"certificatesigningrequests"},
			Verbs:     []string{"create", "get"},
		}},
	})).To(Succeed())
	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: csrRequester},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: csrRequester},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:authenticated"}},
	})).To(Succeed())
}

// getFirstFoundEnvTestBinaryDir locates the first binary in the specified path.
// ENVTEST-based tests depend on specific binaries, usually located in paths set by
// controller-runtime. When running tests directly (e.g., via an IDE) without using
// Makefile targets, the 'BinaryAssetsDirectory' must be explicitly configured.
//
// This function streamlines the process by finding the required binaries, similar to
// setting the 'KUBEBUILDER_ASSETS' environment variable. To ensure the binaries are
// properly set up, run 'make setup-envtest' beforehand.
func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
