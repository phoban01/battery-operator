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

package execagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/battery-operator/internal/clock"
	"github.com/phoban01/battery-operator/internal/hostcheck"
)

const (
	// startRetry is the wait between attempts at reading the Host's Node
	// at start.
	startRetry = 2 * time.Second
	// annotationResync is how often the Node report is written even when
	// nothing the agent knows of has changed.
	annotationResync = time.Minute
	// guardResync is how often the drain guard is checked with the API
	// server even when nothing has changed.
	guardResync = time.Minute
	// stopGrace is how long in-flight requests are given when the agent
	// stops before their streams are cut. A cut stream is a stream failure
	// for the caller, never a finished command (EA-020).
	stopGrace = 5 * time.Second
	// Keepalive of the exec API: callers keep one connection per Host with
	// pings on, which are permitted; the agent pings idle callers in turn
	// so that a caller that vanished does not hold a stream open.
	serverKeepaliveTime    = 30 * time.Second
	serverKeepaliveTimeout = 10 * time.Second
	clientPingMinTime      = 10 * time.Second
)

// Options are everything Run needs. Config, Kube, Claims and Flintlockd are
// required.
type Options struct {
	Config *Config
	// Kube is the API server client, holding no more than
	// config/exec-agent/rbac.yaml grants.
	Kube kubernetes.Interface
	// Claims looks the claims up.
	Claims ClaimLookup
	// Flintlockd is this Host's flintlockd (EA-001); DialFlintlockd makes
	// one.
	Flintlockd *Flintlockd
	// HostAddresses are this Host's addresses, which the configuration is
	// validated against; nil reads them from the Host's interfaces.
	HostAddresses []net.IP
	// Logger defaults to discarding.
	Logger logr.Logger
	// Clock defaults to the real one.
	Clock clock.Clock
	// Listener, when set, is served instead of listening on the Host's
	// address. It is plain TCP; Run serves TLS on it. Tests use it to learn
	// the port.
	Listener net.Listener
	// Ready, when set, is closed once the exec API is served.
	Ready chan<- struct{}
}

// Run is the Exec Agent: it serves the exec API on the Host's internal
// address and keeps its Host's Node report and drain guard up to date
// until ctx ends. Stopping leaves the Node report and the guard as they
// are; the next Run takes them over.
func Run(ctx context.Context, opts Options) error {
	cfg := opts.Config
	if cfg == nil || opts.Kube == nil || opts.Claims == nil || opts.Flintlockd == nil {
		return errors.New("execagent: Run needs a configuration, a Kubernetes client, a claim lookup and a flintlockd client")
	}
	hostAddrs := opts.HostAddresses
	if hostAddrs == nil {
		var err error
		if hostAddrs, err = hostcheck.HostAddresses(); err != nil {
			return err
		}
	}
	if err := cfg.Validate(hostAddrs); err != nil {
		return err
	}
	log := opts.Logger
	if log.GetSink() == nil {
		log = logr.Discard()
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real{}
	}

	// The admission policy tells Hosts apart by the node in the agent's
	// identity (EA-051); without it every change to the Host's Node would be
	// refused, so an agent that does not carry its Host does not start
	// (EA-050).
	if err := CheckIdentity(ctx, opts.Kube, cfg.HostNode); err != nil {
		return err
	}
	host, err := awaitHostNode(logr.NewContext(ctx, log), opts.Kube, cfg.HostNode, clk)
	if err != nil {
		return err
	}
	address := cfg.Address
	if address == "" {
		if address = hostInternalIP(host); address == "" {
			return fmt.Errorf("execagent: the host's node %s has no internal address to serve the exec API on", cfg.HostNode)
		}
	}
	cert, err := newServingCert(cfg.TLS, address, log)
	if err != nil {
		return err
	}
	listener := opts.Listener
	if listener == nil {
		if listener, err = net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(cfg.Port))); err != nil {
			return fmt.Errorf("execagent: listening on %s: %w", address, err)
		}
	}
	defer func() { _ = listener.Close() }()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return err
	}
	agentAddress := net.JoinHostPort(address, port)

	s := &server{
		hostNode:    cfg.HostNode,
		fl:          opts.Flintlockd,
		authn:       &authenticator{reviews: opts.Kube.AuthenticationV1().TokenReviews(), audiences: cfg.TokenAudiences, timeout: cfg.CallTimeout},
		claims:      opts.Claims,
		clk:         clk,
		log:         log,
		openTimeout: cfg.ExecOpenTimeout,
		callTimeout: cfg.CallTimeout,
	}
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(cert.tlsConfig())),
		grpc.ChainUnaryInterceptor(s.unaryInterceptor),
		grpc.ChainStreamInterceptor(s.streamInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: serverKeepaliveTime, Timeout: serverKeepaliveTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: clientPingMinTime, PermitWithoutStream: true}),
	)
	s.register(srv)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()
	defer stopServer(srv, served)
	log.Info("Started serving the exec API", "address", agentAddress, "node", cfg.HostNode)
	if opts.Ready != nil {
		close(opts.Ready)
	}

	return keepReporting(logr.NewContext(ctx, log), opts, clk, agentAddress, served)
}

// keepReporting keeps the Host's Node report and drain guard up to date
// until ctx ends or the exec API stops.
func keepReporting(ctx context.Context, opts Options, clk clock.Clock, agentAddress string, served <-chan error) error {
	cfg, log := opts.Config, logr.FromContextOrDiscard(ctx)
	notes := &annotator{kube: opts.Kube, hostNode: cfg.HostNode, resync: annotationResync}
	guard := &drainGuard{
		cfg: cfg, log: log, clk: clk, kube: opts.Kube, claims: opts.Claims,
		hostNode: cfg.HostNode, resync: guardResync,
	}
	var lastReady *readiness
	for {
		r := checkReadiness(ctx, cfg, opts.Flintlockd)
		if lastReady == nil || *lastReady != r {
			log.Info("Checked the Host's readiness", "ready", r.ready, "reason", r.reason, "message", r.message)
			lastReady = &r
		}
		if err := notes.publish(ctx, nodeReport(r, agentAddress), clk.Now()); err != nil && ctx.Err() == nil {
			log.Error(err, "Failed to publish the Node report", "node", cfg.HostNode)
		}
		if host, err := opts.Kube.CoreV1().Nodes().Get(ctx, cfg.HostNode, metav1.GetOptions{}); err != nil {
			if ctx.Err() == nil {
				log.Error(err, "Failed to read the Host's Node", "node", cfg.HostNode)
			}
		} else if err := guard.reconcile(ctx, host); err != nil && ctx.Err() == nil {
			log.Error(err, "Failed to reconcile the drain guard", "node", cfg.HostNode)
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			return fmt.Errorf("execagent: the exec API stopped: %w", err)
		case <-clk.After(cfg.SyncInterval):
		}
	}
}

// stopServer stops the exec API, giving in-flight requests stopGrace to
// finish and then cutting them.
func stopServer(srv *grpc.Server, served <-chan error) {
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopGrace):
		srv.Stop()
		<-stopped
	}
	select {
	case <-served:
	default:
	}
}

// awaitHostNode reads the Host's Node, waiting for it to exist: the agent
// can start before the Host's kubelet has registered.
func awaitHostNode(ctx context.Context, kube kubernetes.Interface, name string, clk clock.Clock) (*corev1.Node, error) {
	log := logr.FromContextOrDiscard(ctx)
	for {
		host, err := kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return host, nil
		}
		log.Error(err, "Failed to read the Host's Node; retrying", "node", name)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clk.After(startRetry):
		}
	}
}
