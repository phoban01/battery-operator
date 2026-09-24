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

// Command fake-flintlockd serves the fake flintlockd
// (internal/fakeflintlock) over TCP with mutual TLS, as a Host's flintlockd.
// The e2e suite (test/e2e) runs it on every kind node that is a Host, in the
// node's network namespace, in place of the Host Image's flintlockd.
//
// It serves with the files the Exec Agent writes to its Host directory
// (EA-064): tls.crt, tls.key and client-ca.crt. The Host Image starts
// flintlockd once they exist and restarts it when they change; kind has no
// such unit, so this command waits for the files itself and reads them
// again for every connection (fakeflintlock.TLS.Reload). Its MicroVMs live
// in memory and go when it stops.
//
// It is a test double, never part of a release: the Dagger module builds
// its image for the e2e suite only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/phoban01/battery-operator/internal/fakeflintlock"
)

// defaultVersion is the flintlock version ServerInfo reports: battery
// v0.3.3's poolmgrd refuses a Host whose flintlockd is older than v0.15.2,
// or whose version it cannot parse.
const defaultVersion = "v0.15.2"

// The files of EA-064, in the certificate directory.
const (
	certFile     = "tls.crt"
	keyFile      = "tls.key"
	clientCAFile = "client-ca.crt"
)

func main() {
	var (
		listen   string
		certDir  string
		version  string
		name     string
		interval time.Duration
	)
	flag.StringVar(&listen, "listen", "", "The address to serve on, host:port. Required.")
	flag.StringVar(&certDir, "cert-dir", "/etc/battery/flintlockd",
		"The directory the Exec Agent writes tls.crt, tls.key and client-ca.crt to.")
	flag.StringVar(&version, "version", defaultVersion, "The flintlock version ServerInfo reports.")
	flag.StringVar(&name, "name", os.Getenv("NODE_NAME"), "The Host's name, for logs. Defaults to $NODE_NAME.")
	flag.DurationVar(&interval, "poll-interval", time.Second, "How often to look for the certificate files.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts)).WithName("fake-flintlockd")
	if listen == "" {
		log.Error(errors.New("--listen is required"), "Refused to start")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(logr.NewContext(ctx, log), listen, certDir, version, name, interval); err != nil {
		log.Error(err, "Stopped serving")
		os.Exit(1)
	}
}

// run waits for the certificate files in certDir, then serves until ctx
// ends.
func run(ctx context.Context, listen, certDir, version, name string, interval time.Duration) error {
	log := logr.FromContextOrDiscard(ctx)
	files := &fakeflintlock.TLS{
		CertFile:     filepath.Join(certDir, certFile),
		KeyFile:      filepath.Join(certDir, keyFile),
		ClientCAFile: filepath.Join(certDir, clientCAFile),
		Reload:       true,
	}
	log.Info("Waiting for the certificate files", "directory", certDir)
	if err := waitForFiles(ctx, interval, files.KeyFile, files.ClientCAFile, files.CertFile); err != nil {
		return err
	}
	srv := fakeflintlock.New(fakeflintlock.Config{
		Name:        name,
		Listen:      listen,
		Version:     version,
		ExecEnabled: true,
		TLS:         files,
	})
	go func() {
		select {
		case <-srv.Ready():
			log.Info("Serving flintlockd", "address", srv.Addr(), "version", version)
		case <-ctx.Done():
		}
	}()
	return srv.Serve(ctx)
}

// waitForFiles returns once every file in names exists, checking every
// interval, or with ctx's error.
func waitForFiles(ctx context.Context, interval time.Duration, names ...string) error {
	for {
		missing := ""
		for _, n := range names {
			if _, err := os.Stat(n); err != nil {
				missing = n
				break
			}
		}
		if missing == "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", missing, ctx.Err())
		case <-time.After(interval):
		}
	}
}
