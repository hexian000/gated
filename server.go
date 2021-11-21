package gated

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/hexian000/gated/api"
	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/metric"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

const network = "tcp"

type Server struct {
	mu sync.RWMutex

	self   *proto.PeerInfo
	dialer net.Dialer
	peers  map[string]*Peer
	cfg    *Config
	router *Router
	api    *api.Server

	tlsListener  net.Listener
	httpListener net.Listener

	rpcCall    chan *rpc.Call
	shutdownCh chan struct{}
}

func NewServer(cfg *Config) *Server {
	hosts := []string(nil)
	for host := range cfg.main.Hosts {
		hosts = append(hosts, host)
	}
	s := &Server{
		cfg:   cfg,
		peers: make(map[string]*Peer),
		self: &proto.PeerInfo{
			PeerName:   cfg.main.Name,
			ServerName: cfg.main.ServerName,
			Address:    cfg.main.AdvertiseAddr,
			RProxy:     cfg.main.RProxy,
			Hosts:      hosts,
		},
		rpcCall:    make(chan *rpc.Call, 8),
		shutdownCh: make(chan struct{}),
	}
	s.router = NewRouter(cfg.main.Domain, cfg.main.DefaultRoute, s, cfg.main.Hosts)
	s.api = api.New(&api.Config{
		Name:      cfg.main.Name,
		Domain:    cfg.main.Domain,
		Transport: s.router.Transport,
		Timeout:   time.Duration(s.cfg.main.Transport.Timeout) * time.Second,
		Metric:    s,
		Router:    s.router,
		Cluster:   s,
	})
	return s
}

func (s *Server) Serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return
		}
		go s.serve(conn)
	}
}

func (s *Server) LocalPeerName() string {
	return s.cfg.Current().Name
}

func (s *Server) Start() error {
	cfg := s.cfg.Current()
	if cfg.Listen != "" {
		slog.Verbose("tls listen:", cfg.Listen)
		l, err := net.Listen(network, cfg.Listen)
		if err != nil {
			slog.Error("listen:", err)
			return err
		}
		s.tlsListener = l
		go s.Serve(l)
	}
	for _, server := range cfg.Servers {
		peer := &Peer{Info: &proto.PeerInfo{
			Address:    server.Address,
			ServerName: server.ServerName,
		}}
		if err := peer.Dial(s); err != nil {
			slog.Error("bootstrap:", err)
		}
		s.addPeer(peer)
	}
	if cfg.ProxyListen != "" {
		slog.Verbose("http listen:", cfg.ProxyListen)
		l, err := net.Listen(network, cfg.ProxyListen)
		if err != nil {
			slog.Error("listen:", err)
			return err
		}
		s.httpListener = l
		go s.api.Serve(l)
	}
	go s.watchdog()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) {
	panic("TODO")
}

func (s *Server) updateRoute(info *proto.PeerInfo) {
	if info.RProxy != "" && info.RProxy != s.self.PeerName {
		s.router.update(info.Hosts, info.RProxy)
	} else {
		s.router.update(info.Hosts, info.PeerName)
	}
}

func (s *Server) Update(info *proto.PeerInfo) {
	s.merge(info)
	s.updateRoute(info)
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	// fast path
	session, err := func() (*yamux.Session, error) {
		p := s.getPeer(peer)
		if p == nil {
			return nil, fmt.Errorf("unknown peer: %s", peer)
		}
		if !p.IsConnected() {
			return nil, nil
		}
		return p.Session, nil
	}()
	if err != nil {
		return nil, err
	}
	if session != nil {
		return session.Open()
	}

	// slow path
	session, err = func() (*yamux.Session, error) {
		p := s.getPeer(peer)
		if p == nil {
			return nil, fmt.Errorf("unknown peer: %s", peer)
		}
		if err := p.Dial(s); err != nil {
			return nil, err
		}
		return p.Session, nil
	}()
	if err != nil {
		return nil, err
	}
	return session.Open()
}

func (s *Server) serve(tcpConn net.Conn) {
	ctx := util.WithTimeout(s.cfg.Timeout())
	defer util.Cancel(ctx)
	slog.Verbose("serve: setup connection")
	tlsConn := tls.Server(tcpConn, s.cfg.tls)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tcpConn.Close()
		slog.Error("serve:", err)
		return
	}
	s.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Server(tlsConn, s.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		slog.Error("serve:", err)
		return
	}
	rpcClientConn, err := muxConn.Open()
	if err != nil {
		slog.Error("serve:", err)
		return
	}
	rpcServerConn, err := muxConn.Accept()
	if err != nil {
		slog.Error("serve:", err)
		return
	}
	rpcServer := rpc.NewServer()
	rpcClient := jsonrpc.NewClient(rpcClientConn)
	now := time.Now()
	p := &Peer{
		Session:   muxConn,
		Created:   now,
		LastSeen:  now,
		rpcServer: rpcServer,
		rpcClient: rpcClient,
	}
	if err := rpcServer.Register(&RPC{
		server: s,
		router: s.router,
		peer:   p,
	}); err != nil {
		_ = muxConn.Close()
		slog.Error("serve:", err)
		return
	}
	go func() {
		err := s.api.Serve(muxConn)
		slog.Info("serve: closing:", err)
	}()

	slog.Verbose("serve: rpc online")
	rpcServer.ServeCodec(jsonrpc.NewServerCodec(rpcServerConn))
	slog.Info("serve: closing")
	_ = muxConn.Close()
}

func (s *Server) closeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, peer := range s.peers {
		if peer.Session != nil {
			_ = peer.Session.Close()
		}
		peer.Session = nil
		peer.rpcServer = nil
		peer.rpcClient = nil
	}
}

func (s *Server) redialRProxy() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, peer := range s.peers {
		if peer.rproxy && (peer.Session == nil || peer.Session.IsClosed()) {
			go func() {
				if err := peer.Dial(s); err != nil {
					slog.Error("redial rproxy:", err)
				}
			}()
		}
	}
}

func (s *Server) watchdog() {
	const (
		hangCheckInterval = 1 * time.Minute
		syncInterval      = 2 * time.Hour
	)
	ticker := time.NewTicker(hangCheckInterval)
	defer ticker.Stop()
	last := time.Now()
	lastSync := last
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			if now.Sub(last) > 2*hangCheckInterval {
				slog.Warning("system hang detected, closing all sessions")
				s.closeAllSessions()
			} else if now.Sub(lastSync) > syncInterval {
				s.broadcast("RPC.Update", s.ClusterInfo())
				// s.deleteLostPeers()
				lastSync = now
			}
			last = now
			s.redialRProxy()
		case call := <-s.rpcCall:
			if call.Error != nil {
				slog.Errorf("rpc: [%s] %v: %v", call.ServiceMethod, call.Reply, call.Error)
			} else {
				slog.Debugf("rpc: [%s] %v", call.ServiceMethod, call.Reply)
			}
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) CollectMetrics(w *bufio.Writer) {
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Server ===\n\n")
	s.api.CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Stack ===\n\n")
	(&metric.Stack{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")
}
