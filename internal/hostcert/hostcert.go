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

// Package hostcert names the per-Host certificates of ADR 0003: the signer
// names their CertificateSigningRequests use, and the SPIFFE IDs they carry,
// the Exec Agent's serving certificate of ADR 0004 included.
// The Operator, which approves and signs them, and the Exec Agent, which
// requests them, both use it.
package hostcert

import (
	"fmt"
	"regexp"
)

const (
	// ServingSigner is the signer name of a Host's flintlockd serving
	// certificate.
	ServingSigner = "battery.liquidmetal-x.dev/flintlockd-serving"
	// ClientSigner is the signer name of an Exec Agent's flintlockd client
	// certificate.
	ClientSigner = "battery.liquidmetal-x.dev/flintlockd-client"
	// ExecAgentServingSigner is the signer name of an Exec Agent's serving
	// certificate, for its exec API (ADR 0004).
	ExecAgentServingSigner = "battery.liquidmetal-x.dev/exec-agent-serving"

	// NodeNameExtra is the user info extra in which the API server records
	// the Node of the pod a ServiceAccount token is bound to.
	NodeNameExtra = "authentication.kubernetes.io/node-name"

	// CABundleConfigMap is the default name of the ConfigMap, in the
	// Operator's namespace, in which the Operator publishes the CA
	// certificates. ServingCAKey holds the serving CA's certificates, and
	// ClientCAKey the flintlockd client CA's.
	CABundleConfigMap = "flintlockd-ca"
	ServingCAKey      = "serving-ca.crt"
	ClientCAKey       = "client-ca.crt"
)

// trustDomainPattern is the SPIFFE trust domain name grammar: lowercase
// letters, digits, dots, dashes and underscores.
var trustDomainPattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// ValidateTrustDomain reports whether td is a usable SPIFFE trust domain.
func ValidateTrustDomain(td string) error {
	if td == "" {
		return fmt.Errorf("no SPIFFE trust domain is configured")
	}
	if len(td) > 255 || !trustDomainPattern.MatchString(td) {
		return fmt.Errorf("%q is not a SPIFFE trust domain: use lowercase letters, digits, dots, dashes and underscores", td)
	}
	return nil
}

// HostID is the SPIFFE ID of the flintlockd on Node node.
func HostID(trustDomain, node string) string {
	return "spiffe://" + trustDomain + "/flintlock/host/" + node
}

// ExecAgentID is the SPIFFE ID of the Exec Agent on Node node.
func ExecAgentID(trustDomain, node string) string {
	return "spiffe://" + trustDomain + "/flintlock/client/exec-agent/" + node
}

// BatteryID is the SPIFFE ID of battery's flintlockd client certificate,
// which cert-manager issues from the flintlockd client CA (CT-023).
func BatteryID(trustDomain string) string {
	return "spiffe://" + trustDomain + "/flintlock/client/battery"
}
