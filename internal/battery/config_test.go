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

package battery

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

//= docs/requirements/06-deployment.md#battery-connection
//= type=test
//# The Operator SHALL give `ClaimVM` a deadline of its own,
//# configurable apart from the deadline of the other unary calls of DP-010.

// TestDeadlineInterceptorGivesClaimVMItsOwnDeadline covers DP-013.
func TestDeadlineInterceptorGivesClaimVMItsOwnDeadline(t *testing.T) {
	interceptor := deadlineInterceptor(time.Second, time.Hour)
	var got time.Time
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		got, _ = ctx.Deadline()
		return nil
	}

	for method, want := range map[string]time.Duration{
		poolmgrv1.Lease_ClaimVM_FullMethodName:   time.Hour,
		poolmgrv1.Lease_Heartbeat_FullMethodName: time.Second,
		poolmgrv1.Lease_ReleaseVM_FullMethodName: time.Second,
	} {
		before := time.Now()
		if err := interceptor(context.Background(), method, nil, nil, nil, invoker); err != nil {
			t.Fatal(err)
		}
		if got.Before(before.Add(want)) || got.After(time.Now().Add(want)) {
			t.Errorf("%s got deadline %v, want %v from now", method, got.Sub(before), want)
		}
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var cfg Config
	cfg.BindFlags(fs)
	if err := fs.Parse([]string{"--battery-call-timeout=3s", "--battery-claim-timeout=5m"}); err != nil {
		t.Fatal(err)
	}
	if cfg.CallTimeout != 3*time.Second || cfg.ClaimTimeout != 5*time.Minute {
		t.Errorf("flags gave CallTimeout %v and ClaimTimeout %v, want 3s and 5m", cfg.CallTimeout, cfg.ClaimTimeout)
	}
	if d := (Config{}).withDefaults().ClaimTimeout; d != DefaultClaimTimeout {
		t.Errorf("default ClaimTimeout = %v, want %v", d, DefaultClaimTimeout)
	}
}

func TestDeadlineInterceptorKeepsAnEarlierDeadline(t *testing.T) {
	interceptor := deadlineInterceptor(time.Hour, time.Hour)
	var got time.Time
	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		got, _ = ctx.Deadline()
		return nil
	}

	before := time.Now()
	if err := interceptor(context.Background(), "/m", nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	if got.Before(before.Add(time.Hour)) || got.After(time.Now().Add(time.Hour)) {
		t.Errorf("a call without a deadline got deadline %v, want an hour from now", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, _ := ctx.Deadline()
	if err := interceptor(ctx, "/m", nil, nil, nil, invoker); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("a call with an earlier deadline got %v, want its own %v", got, want)
	}
}

func TestTransportCredentials(t *testing.T) {
	dir := t.TempDir()
	certs, err := fakeflintlock.WriteTestCerts(dir)
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("no certificate here"), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := transportCredentials(TLSConfig{})
	if err != nil || creds.Info().SecurityProtocol != "insecure" {
		t.Errorf("no TLS material: got %v, %v, want plaintext", creds, err)
	}
	creds, err = transportCredentials(TLSConfig{CAFile: certs.CAFile, CertFile: certs.ClientCertFile, KeyFile: certs.ClientKeyFile})
	if err != nil || creds.Info().SecurityProtocol != "tls" {
		t.Errorf("CA and client certificate: got %v, %v, want TLS", creds, err)
	}
	if _, err := transportCredentials(TLSConfig{CAFile: empty}); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Errorf("a CA file without a certificate: got %v, want it refused", err)
	}
	if _, err := transportCredentials(TLSConfig{CAFile: filepath.Join(dir, "missing.pem")}); err == nil {
		t.Error("a missing CA file was accepted")
	}

	cfg := Config{Address: DefaultAddress, TLS: TLSConfig{CertFile: certs.ClientCertFile}}
	if err := cfg.Validate(); err == nil {
		t.Error("a client certificate without its key was accepted")
	}
}
