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

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// TestRunWaitsForTheCertificates: run serves nothing until the Exec
// Agent's three files exist, then serves mutual TLS with them and reports a
// version poolmgrd accepts.
func TestRunWaitsForTheCertificates(t *testing.T) {
	certs, err := fakeflintlock.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	dir := t.TempDir()
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, logr.Discard(), addr, dir, defaultVersion, "h1", 10*time.Millisecond) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// Nothing listens before the files are there.
	time.Sleep(100 * time.Millisecond)
	if c, err := net.Dial("tcp", addr); err == nil {
		_ = c.Close()
		t.Fatal("listening before the certificate files exist")
	}

	// The Exec Agent writes tls.crt last.
	for _, f := range [][2]string{
		{certs.ServerKeyFile, keyFile},
		{certs.CAFile, clientCAFile},
		{certs.ServerCertFile, certFile},
	} {
		src, dst := f[0], f[1]
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	caPEM, err := os.ReadFile(certs.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	client, err := tls.LoadX509KeyPair(certs.ClientCertFile, certs.ClientKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: roots, Certificates: []tls.Certificate{client}, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	var resp *mvmv1.ServerInfoResponse
	deadline := time.Now().Add(10 * time.Second)
	for {
		callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
		resp, err = mvmv1.NewMicroVMClient(conn).ServerInfo(callCtx, &emptypb.Empty{})
		callCancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("ServerInfo: %v", err)
	}
	if got := resp.GetVersion().GetVersion(); got != "v0.15.2" {
		t.Errorf("version = %q, want v0.15.2", got)
	}
	if !resp.GetExec().GetEnabled() {
		t.Error("exec is disabled; the Exec Agent reports such a Host not ready")
	}
}

// freeAddr is a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
