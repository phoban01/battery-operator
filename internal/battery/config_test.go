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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

func TestDeadlineInterceptorKeepsAnEarlierDeadline(t *testing.T) {
	interceptor := deadlineInterceptor(time.Hour)
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
