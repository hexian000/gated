package gated

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/hexian000/gated/metric"
	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/proxy"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

const network = "tcp"

type Server struct {
	mu         sync.RWMutex
	peers      map[string]*peer
	cfg        *Config
	router     *Router
	handler    http.Handler
	forwarder  *proxy.Forwarder
	canceller  *util.Canceller
	httpServer *http.Server
	httpClient *http.Client

	tlsListener  net.Listener
	httpListener net.Listener

	shutdownCh chan struct{}
}

func NewServer(cfg *Config) *Server {
	timeout := cfg.Timeout()
	s := &Server{
		cfg:        cfg,
		peers:      make(map[string]*peer),
		forwarder:  proxy.NewForwarder(),
		canceller:  util.NewCanceller(),
		shutdownCh: make(chan struct{}),
		httpServer: &http.Server{
			ReadHeaderTimeout: timeout,
			ErrorLog:          newHTTPLogger(),
		},
	}
	s.handler = newAPIHandler(s, nil)
	s.router = NewRouter(cfg.main.Domain, cfg.main.DefaultRoute, s, cfg.main.Hosts)
	s.httpServer.Handler = s.handler
	s.httpClient = &http.Client{
		Transport: s.router.Transport,
		Timeout:   timeout,
	}
	return s
}

func (s *Server) Serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				time.Sleep(2 * time.Second)
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

func (s *Server) bootstrap(addr, sni string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout())
	defer cancel()
	peer := newPeer(s)
	cluster, err := peer.Bootstrap(ctx, s, addr, sni)
	if err != nil {
		slog.Errorf("bootstrap %s: %v", addr, err)
		return
	}
	s.addPeer(peer)
	s.MergeCluster(cluster)
	slog.Verbosef("bootstrap %s[%s]: ok", addr, peer.info.PeerName)
	s.print()
	s.router.print()
}

func (s *Server) Start() error {
	cfg := s.cfg.Current()
	if cfg.Listen != "" {
		slog.Info("tls listen:", cfg.Listen)
		l, err := net.Listen(network, cfg.Listen)
		if err != nil {
			slog.Error("listen:", err)
			return err
		}
		s.tlsListener = l
		go s.Serve(l)
	}
	for _, server := range cfg.Servers {
		go s.bootstrap(server.Address, server.ServerName)
	}
	if cfg.HTTPListen != "" {
		slog.Info("http listen:", cfg.HTTPListen)
		l, err := net.Listen(network, cfg.HTTPListen)
		if err != nil {
			slog.Error("listen:", err)
			return err
		}
		s.httpListener = l
		go s.httpServer.Serve(l)
	}
	go s.watchdog()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) {
	panic("TODO")
}

func (s *Server) FindProxy(peer string) (string, error) {
	proxy := ""
	for result := range s.broadcast("RPC.Ping", &proto.Ping{
		Source:      s.LocalPeerName(),
		Destination: peer,
		TTL:         1,
	}, reflect.TypeOf(proto.Ping{})) {
		if result.err != nil {
			slog.Warning("ping:", result.err)
			continue
		}
		reply := result.reply.(*proto.Ping)
		proxy = reply.Source
		break
	}
	if proxy == "" {
		return "", fmt.Errorf("ping: %s is unreachable", peer)
	}
	return proxy, nil
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	p := s.getPeer(peer)
	conn, err := p.DialContext(ctx)
	if err == nil {
		return conn, nil
	}
	slog.Debug("peer dial:", err)
	proxy, err := s.FindProxy(peer)
	if err != nil {
		slog.Debug("find proxy:", err)
		return nil, err
	}
	slog.Debugf("dial peer %s via %s", peer, proxy)
	p = s.getPeer(proxy)
	return p.DialContext(ctx)
}

func (s *Server) serve(tcpConn net.Conn) {
	connId := tcpConn.RemoteAddr()
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	slog.Verbosef("serve %v: setup connection", connId)
	tlsConn := tls.Server(tcpConn, s.cfg.tls)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		slog.Errorf("serve %v: %v", connId, err)
		_ = tcpConn.Close()
		return
	}
	s.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Server(tlsConn, s.cfg.MuxConfig())
	if err != nil {
		slog.Errorf("serve %v: %v", connId, err)
		_ = tlsConn.Close()
		return
	}
	p := newPeer(s)
	p.Serve(s, muxConn)
}

func (s *Server) closeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, peer := range s.peers {
		_ = peer.Close()
	}
}

func (s *Server) redial() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.peers {
		if p.isReachable() && !p.isConnected() {
			go s.bootstrap(p.info.Address, p.info.ServerName)
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
				_ = s.broadcast("RPC.Sync", s.ClusterInfo(), reflect.TypeOf(proto.None{}))
				// s.deleteLostPeers()
				lastSync = now
			}
			last = now
			if s.cfg.Current().AdvertiseAddr == "" {
				s.redial()
			}
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) CollectMetrics(w *bufio.Writer) {
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Peers ===\n\n")
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for name, peer := range s.peers {
			w.WriteString(fmt.Sprintf("%s: %v\n", name, peer.isConnected()))
		}
	}()
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Server ===\n\n")
	s.forwarder.CollectMetrics(w)
	s.canceller.CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Stack ===\n\n")
	(&metric.Stack{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")
}
