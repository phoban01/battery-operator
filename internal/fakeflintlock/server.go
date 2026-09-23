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

package fakeflintlock

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	mvmv1 "github.com/liquidmetal-dev/flintlock/api/services/microvm/v1alpha1"
	execv1 "github.com/liquidmetal-dev/flintlock/api/services/microvmexec/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/phoban01/battery-operator/internal/clock"
)

const (
	// defaultListen is where Serve listens when Config.Listen is empty: a
	// free loopback port, which is what every test wants.
	defaultListen = "127.0.0.1:0"
	// gracefulStopTimeout bounds how long Serve lets in-flight RPCs finish
	// after the Server has been closed before it tears the connections down.
	gracefulStopTimeout = 5 * time.Second
	// memBuffer is the in-memory listener's buffer size.
	memBuffer = 1 << 20
)

// errClosed is the cause of every failure after the Server has been closed.
var errClosed = errors.New("fake flintlockd closed")

// Config is the static configuration of a Server. Every field is optional.
type Config struct {
	// Name labels the Server in errors and in the vsock paths it reports.
	Name string
	// Listen is the TCP address Serve binds; empty means a free loopback
	// port.
	Listen string
	// Version is the flintlock version ServerInfo reports.
	Version string
	// ExecEnabled is the exec flag ServerInfo reports. The MicroVMExec
	// service is served either way.
	ExecEnabled bool
	// BootDelay is how long a new MicroVM stays PENDING before it is
	// CREATED, measured on Clock. Zero creates it at once.
	BootDelay time.Duration
	// Clock drives boots and ServerInfo's uptime; nil is the wall clock.
	Clock clock.Clock
	// TLS, when set, makes Serve listen with TLS.
	TLS *TLS
}

// TLS is the Server's TLS material for Serve. The in-memory Conn is always
// plaintext.
type TLS struct {
	CertFile string
	KeyFile  string
	// ClientCAFile, when set, makes the Server require and verify a client
	// certificate signed by it: mutual TLS, as battery reaches flintlockd.
	ClientCAFile string
}

// Faults are the Server's runtime fault switches. All false is a healthy
// flintlockd. They are read where they act and may change at any time.
type Faults struct {
	// CreateFails ends every boot that completes while it is set in FAILED
	// rather than CREATED.
	CreateFails bool
	// DropExecBeforeExit ends every exec stream with UNAVAILABLE after its
	// output and before its exit status, which is what a Host dying under a
	// running command looks like to the client.
	DropExecBeforeExit bool
	// Unresponsive makes the Server stop answering without closing its
	// listener: new calls wait until the fault is cleared or their caller
	// gives up, and running exec streams stop delivering output.
	Unresponsive bool
}

// Server is one fake flintlockd. Build it with New.
type Server struct {
	cfg     Config
	clk     clock.Clock
	started time.Time

	// ctx is the Server's lifetime; cancel ends it. wg counts the
	// goroutines Close waits for: pending boots and running execs.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mem is the gRPC server behind Conn, built by New and never replaced.
	mem      *grpc.Server
	memLis   *bufconn.Listener
	stopOnce sync.Once

	// mu guards everything below it.
	mu     sync.Mutex
	vms    map[string]*microVM
	execs  []*execv1.ExecStart
	exec   ExecFunc
	addr   string
	ready  chan struct{}
	served bool
	closed bool

	// faultsMu guards faults and faultsCh. faultsCh is closed and replaced
	// on every SetFaults, so that a goroutine held by Unresponsive wakes up
	// when a test clears it.
	faultsMu sync.Mutex
	faults   Faults
	faultsCh chan struct{}
}

// New builds a Server from cfg. It is usable through Conn at once; Serve
// adds a TCP listener.
func New(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Name == "" {
		cfg.Name = "flintlockd"
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:      cfg,
		clk:      cfg.Clock,
		ctx:      ctx,
		cancel:   cancel,
		vms:      make(map[string]*microVM),
		ready:    make(chan struct{}),
		faultsCh: make(chan struct{}),
	}
	s.started = s.clk.Now()
	s.memLis = bufconn.Listen(memBuffer)
	s.mem = s.newGRPCServer()
	go func() { _ = s.mem.Serve(s.memLis) }()
	return s
}

//= docs/requirements/08-test-doubles.md#fake-flintlockd
//# The fake `flintlockd` SHALL serve flintlock's MicroVM and
//# `MicroVMExec` services over gRPC, with MicroVMs that exist only in memory.

// newGRPCServer builds a gRPC server with the MicroVM and MicroVMExec
// services registered from flintlock's generated stubs, behind the
// Unresponsive fault. Serve and the in-memory listener each get one.
func (s *Server) newGRPCServer(opts ...grpc.ServerOption) *grpc.Server {
	opts = append(opts,
		grpc.ChainUnaryInterceptor(s.unaryInterceptor),
		grpc.ChainStreamInterceptor(s.streamInterceptor),
		// A client keeps one connection with keepalive on; permit its pings
		// so that they are not answered with GOAWAY too_many_pings.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             time.Second,
			PermitWithoutStream: true,
		}),
	)
	srv := grpc.NewServer(opts...)
	mvmv1.RegisterMicroVMServer(srv, &microVMService{s: s})
	execv1.RegisterMicroVMExecServer(srv, &execService{s: s})
	return srv
}

// Config returns the configuration the Server was built with, defaults
// applied.
func (s *Server) Config() Config { return s.cfg }

// Conn returns a new client connection to the Server over an in-memory
// listener. opts are added after the Server's own, so a test can add
// interceptors. The caller closes the connection.
func (s *Server) Conn(opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	lis := s.memLis
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)
	conn, err := grpc.NewClient("passthrough:///"+s.cfg.Name, opts...)
	if err != nil {
		return nil, fmt.Errorf("fake flintlockd %s: in-memory connection: %w", s.cfg.Name, err)
	}
	return conn, nil
}

// Serve listens on Config.Listen and serves the MicroVM and MicroVMExec
// services, with TLS when configured, until ctx is cancelled or the Server
// is closed. Ready is closed once the listener is bound. On the way out it
// closes the Server. It returns nil after a clean shutdown and the bind or
// TLS error otherwise. It can be called once.
func (s *Server) Serve(ctx context.Context) error {
	creds, err := s.serverCredentials()
	if err != nil {
		return err
	}
	lis, err := s.listen()
	if err != nil {
		return err
	}
	srv := s.newGRPCServer(grpc.Creds(creds))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	select {
	case <-ctx.Done():
	case <-s.ctx.Done():
	case err := <-serveErr:
		_ = s.Close()
		return fmt.Errorf("fake flintlockd %s: serve: %w", s.cfg.Name, err)
	}

	// Close first: it ends every exec, so GracefulStop has only unary
	// calls to wait for and the timeout below is a backstop.
	_ = s.Close()
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(gracefulStopTimeout):
		srv.Stop()
		<-stopped
	}
	<-serveErr
	return nil
}

// listen binds Serve's listener and publishes Addr.
func (s *Server) listen() (net.Listener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	if s.served {
		return nil, errors.New("fake flintlockd: Serve called twice")
	}
	addr := s.cfg.Listen
	if addr == "" {
		addr = defaultListen
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("fake flintlockd %s: listen: %w", s.cfg.Name, err)
	}
	s.served = true
	s.addr = lis.Addr().String()
	close(s.ready)
	return lis, nil
}

// serverCredentials builds Serve's transport credentials from Config.TLS:
// plaintext when unset, otherwise the server certificate, and client
// certificate verification when ClientCAFile is set too.
func (s *Server) serverCredentials() (credentials.TransportCredentials, error) {
	if s.cfg.TLS == nil {
		return insecure.NewCredentials(), nil
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("fake flintlockd %s: loading server certificate: %w", s.cfg.Name, err)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if s.cfg.TLS.ClientCAFile != "" {
		pem, err := os.ReadFile(s.cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("fake flintlockd %s: reading client CA: %w", s.cfg.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("fake flintlockd %s: no certificates in client CA file %s", s.cfg.Name, s.cfg.TLS.ClientCAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tlsCfg), nil
}

// Addr is the bound TCP address once Serve is listening, empty before.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Ready is closed once Serve is listening, at which point Addr is set. It
// never closes if Serve fails before binding; wait on Serve's result too.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Close ends the Server: it cancels every running exec, stops every
// pending boot, waits for both and stops the in-memory listener. Every
// later call fails with UNAVAILABLE. It is idempotent, and it is also what
// Serve does on its way out.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.stopOnce.Do(s.mem.Stop)
	return nil
}

// SetFaults replaces the fault switches. Clearing Unresponsive wakes
// whatever it was holding.
func (s *Server) SetFaults(f Faults) {
	s.faultsMu.Lock()
	defer s.faultsMu.Unlock()
	s.faults = f
	close(s.faultsCh)
	s.faultsCh = make(chan struct{})
}

// Faults returns the current fault switches.
func (s *Server) Faults() Faults {
	s.faultsMu.Lock()
	defer s.faultsMu.Unlock()
	return s.faults
}

// faultsAndSignal returns the current faults and a channel that is closed
// the next time they change.
func (s *Server) faultsAndSignal() (Faults, <-chan struct{}) {
	s.faultsMu.Lock()
	defer s.faultsMu.Unlock()
	return s.faults, s.faultsCh
}

// awaitResponsive blocks while the Unresponsive fault is set. It returns
// nil once the Server answers again, ctx.Err() when the caller gives up
// first and errClosed when the Server is closed meanwhile. That makes the
// fault a timeout at the caller rather than a refused connection.
func (s *Server) awaitResponsive(ctx context.Context) error {
	for {
		faults, changed := s.faultsAndSignal()
		if !faults.Unresponsive {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return errClosed
		}
	}
}

// admit holds a call while the Server is Unresponsive and returns a status
// error when the call is not to be served.
func (s *Server) admit(ctx context.Context) error {
	if err := s.awaitResponsive(ctx); err != nil {
		if errors.Is(err, errClosed) {
			return errClosedStatus()
		}
		return status.FromContextError(err).Err()
	}
	return nil
}

func (s *Server) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.admit(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.admit(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

// serverInfo builds the ServerInfo response: the configured version, the
// uptime since New on the Server's clock, the exec flag with the listen
// address when enabled, and the SSH proxy disabled.
func (s *Server) serverInfo() (*mvmv1.ServerInfoResponse, error) {
	s.mu.Lock()
	closed, addr := s.closed, s.addr
	s.mu.Unlock()
	if closed {
		return nil, errClosedStatus()
	}
	uptime := max(s.clk.Now().Sub(s.started), 0)
	resp := &mvmv1.ServerInfoResponse{
		Version:  &mvmv1.VersionInfo{Version: s.cfg.Version, BuildDate: "fake", CommitHash: "fake"},
		Uptime:   durationpb.New(uptime),
		Exec:     &mvmv1.GuestAgentServiceInfo{Enabled: s.cfg.ExecEnabled},
		SshProxy: &mvmv1.GuestAgentServiceInfo{},
	}
	if s.cfg.ExecEnabled {
		resp.Exec.Address = addr
	}
	return resp, nil
}

// microVMService serves microvm.services.api.v1alpha1.MicroVM.
type microVMService struct {
	mvmv1.UnimplementedMicroVMServer
	s *Server
}

// CreateMicroVM implements MicroVMServer.
func (m *microVMService) CreateMicroVM(_ context.Context, req *mvmv1.CreateMicroVMRequest) (*mvmv1.CreateMicroVMResponse, error) {
	vm, err := m.s.createMicroVM(req.GetMicrovm())
	if err != nil {
		return nil, err
	}
	return &mvmv1.CreateMicroVMResponse{Microvm: vm}, nil
}

// DeleteMicroVM implements MicroVMServer.
func (m *microVMService) DeleteMicroVM(_ context.Context, req *mvmv1.DeleteMicroVMRequest) (*emptypb.Empty, error) {
	if err := m.s.deleteMicroVM(req.GetUid()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// GetMicroVM implements MicroVMServer.
func (m *microVMService) GetMicroVM(_ context.Context, req *mvmv1.GetMicroVMRequest) (*mvmv1.GetMicroVMResponse, error) {
	vm, err := m.s.getMicroVM(req.GetUid())
	if err != nil {
		return nil, err
	}
	return &mvmv1.GetMicroVMResponse{Microvm: vm}, nil
}

// ListMicroVMs implements MicroVMServer.
func (m *microVMService) ListMicroVMs(_ context.Context, req *mvmv1.ListMicroVMsRequest) (*mvmv1.ListMicroVMsResponse, error) {
	vms, err := m.s.listMicroVMs(req.GetNamespace(), req.Name)
	if err != nil {
		return nil, err
	}
	return &mvmv1.ListMicroVMsResponse{Microvm: vms}, nil
}

// ListMicroVMsStream implements MicroVMServer.
func (m *microVMService) ListMicroVMsStream(req *mvmv1.ListMicroVMsRequest, stream grpc.ServerStreamingServer[mvmv1.ListMessage]) error {
	vms, err := m.s.listMicroVMs(req.GetNamespace(), req.Name)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if err := stream.Send(&mvmv1.ListMessage{Microvm: vm}); err != nil {
			return err
		}
	}
	return nil
}

// ServerInfo implements MicroVMServer.
func (m *microVMService) ServerInfo(context.Context, *emptypb.Empty) (*mvmv1.ServerInfoResponse, error) {
	return m.s.serverInfo()
}

// execService serves microvmexec.services.api.v1alpha1.MicroVMExec.
type execService struct {
	execv1.UnimplementedMicroVMExecServer
	s *Server
}

// ExecCommand implements MicroVMExecServer.
func (e *execService) ExecCommand(stream grpc.BidiStreamingServer[execv1.ExecCommandRequest, execv1.ExecCommandResponse]) error {
	return e.s.execCommand(stream)
}

// CreateMicroVM creates a MicroVM directly, without a connection, for a
// test that needs one to exist. It behaves as the RPC does and returns its
// status errors.
func (s *Server) CreateMicroVM(spec *types.MicroVMSpec) (*types.MicroVM, error) {
	return s.createMicroVM(spec)
}

// MicroVMs returns a copy of every MicroVM the Server holds, sorted by uid.
func (s *Server) MicroVMs() []*types.MicroVM {
	vms, err := s.listMicroVMs("", nil)
	if err != nil {
		return nil
	}
	return vms
}

// Compile-time checks that the services implement the generated servers.
var (
	_ mvmv1.MicroVMServer      = (*microVMService)(nil)
	_ execv1.MicroVMExecServer = (*execService)(nil)
)
