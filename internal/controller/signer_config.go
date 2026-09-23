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
	"errors"
	"flag"
	"os"
	"strings"
	"time"

	"github.com/phoban01/battery-operator/internal/hostcert"
)

// The default names of the CA Secrets and of the CA bundle ConfigMap. The
// RBAC markers grant access to these names only, so the Manifests have to use
// them.
const (
	DefaultServingCASecret   = "flintlockd-serving-ca"
	DefaultClientCASecret    = "flintlockd-client-ca"
	DefaultCABundleConfigMap = hostcert.CABundleConfigMap
)

// SignerConfig is the Operator's configuration for approving and signing the
// Hosts' certificates.
type SignerConfig struct {
	// TrustDomain is the SPIFFE trust domain every certificate is issued
	// under.
	TrustDomain string
	// MaxDuration caps every certificate's validity.
	MaxDuration time.Duration
	// Namespace is the Operator's namespace, which holds the CA Secrets and
	// the CA bundle ConfigMap.
	Namespace string
	// ServingCASecret and ClientCASecret name the CA Secrets, which have
	// cert-manager's kubernetes.io/tls layout.
	ServingCASecret string
	ClientCASecret  string
	// CABundleConfigMap names the ConfigMap the CA certificates are
	// published in.
	CABundleConfigMap string
	// ExecAgentNamespace and ExecAgentServiceAccount name the Exec Agent's
	// ServiceAccount, the only requester whose requests are approved.
	ExecAgentNamespace      string
	ExecAgentServiceAccount string
}

// BindFlags binds c to the Operator's command line flags. Call Complete
// after parsing them.
func (c *SignerConfig) BindFlags(fs *flag.FlagSet) {
	fs.StringVar(&c.TrustDomain, "trust-domain", "",
		"The SPIFFE trust domain of the Hosts' certificates. Required.")
	fs.DurationVar(&c.MaxDuration, "certificate-max-duration", 7*24*time.Hour,
		"The longest validity of a Host certificate the Operator signs.")
	fs.StringVar(&c.Namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"The Operator's namespace, which holds the CA Secrets and the CA bundle ConfigMap. Defaults to $POD_NAMESPACE.")
	fs.StringVar(&c.ServingCASecret, "serving-ca-secret", DefaultServingCASecret,
		"The Secret of the serving CA. The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ClientCASecret, "client-ca-secret", DefaultClientCASecret,
		"The Secret of the flintlockd client CA. The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.CABundleConfigMap, "ca-bundle-configmap", DefaultCABundleConfigMap,
		"The ConfigMap the CA certificates are published in. The Operator's RBAC grants access to the default name only.")
	fs.StringVar(&c.ExecAgentServiceAccount, "exec-agent-service-account", "exec-agent",
		"The Exec Agent's ServiceAccount, as [namespace/]name. The namespace defaults to the Operator's.")
}

// Complete fills in what the flags leave implicit, and validates c.
func (c *SignerConfig) Complete() error {
	if ns, name, ok := strings.Cut(c.ExecAgentServiceAccount, "/"); ok {
		c.ExecAgentNamespace, c.ExecAgentServiceAccount = ns, name
	}
	if c.ExecAgentNamespace == "" {
		c.ExecAgentNamespace = c.Namespace
	}
	return c.Validate()
}

// Validate reports the first thing wrong with c.
func (c SignerConfig) Validate() error {
	//= docs/requirements/09-certificates.md#spiffe-identity
	//# The Operator SHALL take its SPIFFE trust domain from its
	//# configuration, and SHALL refuse to start without one.
	if err := hostcert.ValidateTrustDomain(c.TrustDomain); err != nil {
		return err
	}
	switch {
	case c.MaxDuration <= 0:
		return errors.New("the maximum certificate duration has to be positive")
	case c.Namespace == "":
		return errors.New("the Operator's namespace is not configured")
	case c.ServingCASecret == "" || c.ClientCASecret == "" || c.CABundleConfigMap == "":
		return errors.New("the CA Secrets and the CA bundle ConfigMap have to be named")
	case c.ExecAgentNamespace == "" || c.ExecAgentServiceAccount == "":
		return errors.New("the Exec Agent's ServiceAccount is not configured")
	}
	return nil
}

// execAgentUsername is the user name the API server gives the Exec Agent's
// ServiceAccount.
func (c SignerConfig) execAgentUsername() string {
	return "system:serviceaccount:" + c.ExecAgentNamespace + ":" + c.ExecAgentServiceAccount
}
