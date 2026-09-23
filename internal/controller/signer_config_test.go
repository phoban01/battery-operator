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
	"flag"
	"testing"
)

// nsFlag sets the Operator's namespace.
const nsFlag = "--namespace=ops"

func TestSignerConfigRequiresATrustDomain(t *testing.T) {
	//= docs/requirements/09-certificates.md#spiffe-identity
	//= type=test
	//# The Operator SHALL take its SPIFFE trust domain from its
	//# configuration, and SHALL refuse to start without one.
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "no trust domain", args: []string{nsFlag}, wantErr: true},
		{name: "empty trust domain", args: []string{nsFlag, "--trust-domain="}, wantErr: true},
		{name: "not a trust domain", args: []string{nsFlag, "--trust-domain=spiffe://Example.org/"}, wantErr: true},
		{name: "a trust domain", args: []string{nsFlag, "--trust-domain=example.org"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POD_NAMESPACE", "")
			var c SignerConfig
			fs := flag.NewFlagSet("operator", flag.ContinueOnError)
			c.BindFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			err := c.Complete()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Complete() = %v, want error: %t", err, tc.wantErr)
			}
			if err == nil && c.TrustDomain != "example.org" {
				t.Errorf("TrustDomain = %q, want example.org", c.TrustDomain)
			}
		})
	}
}

func TestSignerConfigExecAgentServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		flag, wantNamespace, wantName string
	}{
		{flag: "", wantNamespace: "ops", wantName: "exec-agent"},
		{flag: "--exec-agent-service-account=agent", wantNamespace: "ops", wantName: "agent"},
		{flag: "--exec-agent-service-account=hosts/agent", wantNamespace: "hosts", wantName: "agent"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			var c SignerConfig
			fs := flag.NewFlagSet("operator", flag.ContinueOnError)
			c.BindFlags(fs)
			args := []string{nsFlag, "--trust-domain=example.org"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := c.Complete(); err != nil {
				t.Fatal(err)
			}
			if c.ExecAgentNamespace != tc.wantNamespace || c.ExecAgentServiceAccount != tc.wantName {
				t.Errorf("Exec Agent = %s/%s, want %s/%s",
					c.ExecAgentNamespace, c.ExecAgentServiceAccount, tc.wantNamespace, tc.wantName)
			}
			want := "system:serviceaccount:" + tc.wantNamespace + ":" + tc.wantName
			if got := c.execAgentUsername(); got != want {
				t.Errorf("execAgentUsername() = %q, want %q", got, want)
			}
		})
	}
}
