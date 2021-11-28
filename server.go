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

func (s *Server) bootstrap(ctx context.Context, p *peer) error {
	addr := p.info.Address
	cluster, err := p.Dial(ctx)
	if err != nil {
		slog.Errorf("bootstrap %s: %v", addr, err)
		return err
	}
	s.addPeer(p)
	s.MergeCluster(cluster)
	slog.Infof("bootstrap %s[%s]: ok", p.info.Address, p.info.PeerName)
	return nil
}

func (s *Server) Start() error {
	cfg := s.cfg.Current()
	timeout := time.Duration(cfg.Timeout) * time.Second
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
		p := newPeer(s)
		p.info.Address = server.Address
		p.info.ServerName = server.ServerName
		go func() {
			ctx := s.canceller.WithTimeout(timeout)
			defer s.canceller.Cancel(ctx)
			if err := s.bootstrap(ctx, p); err != nil {
				slog.Error("start:", err)
			}
		}()
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
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	for result := range s.Broadcast(ctx, "RPC.Ping", &proto.Ping{
		Source:      s.LocalPeerName(),
		Destination: peer,
		TTL:         2,
	}, reflect.TypeOf(proto.Ping{})) {
		if result.err != nil {
			slog.Warning("ping:", result.err)
			continue
		}
		proxy = result.from.info.PeerName
		break
	}
	if proxy == "" {
		return "", fmt.Errorf("ping: %s is unreachable", peer)
	}
	return proxy, nil
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	p := s.getPeer(peer)
	if p != nil {
		conn, err := p.DialContext(ctx)
		if err == nil {
			return conn, nil
		}
		slog.Debug("peer dial:", err)
	}
	proxy, err := s.FindProxy(peer)
	if err != nil {
		slog.Debug("find proxy:", err)
		return nil, err
	}
	slog.Debugf("dial peer %s via %s", peer, proxy)
	p = s.getPeer(proxy)
	if p == nil {
		return nil, fmt.Errorf("unknown peer: %s", proxy)
	}
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
	p.Serve(muxConn)
}

func (s *Server) closeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, peer := range s.peers {
		_ = peer.Close()
	}
}

func (s *Server) maintenance() {
	cfg := s.cfg.Current()
	needRedial := cfg.AdvertiseAddr == ""
	idleTimeout := time.Duration(cfg.Transport.IdleTimeout) * time.Second
	for _, p := range s.getPeers() {
		if !p.isReachable() {
			continue
		}
		if p.isConnected() {
			p.checkNumStreams()
		} else if needRedial {
			slog.Infof("redial %q: %q", p.info.PeerName, p.info.Address)
			go func(p *peer) {
				ctx := s.canceller.WithTimeout(s.cfg.Timeout())
				defer s.canceller.Cancel(ctx)
				if err := s.bootstrap(ctx, p); err != nil {
					slog.Errorf("redial %q: %v", p.info.PeerName, err)
				}
			}(p)
		}
		if time.Since(p.lastSeen) > idleTimeout {
			slog.Infof("idle timeout expired: %s", p.info.PeerName)
			_ = p.Close()
		}
	}
}

func (s *Server) sync() {
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	var cluster proto.Cluster
	err := s.Gossip(ctx, "RPC.Sync", s.ClusterInfo(), &cluster)
	if err != nil {
		slog.Warning("gossip:", err)
	}
	s.MergeCluster(&cluster)
	slog.Debug("sync finished with", cluster.Self.PeerName)
}

func (s *Server) watchdog() {
	const (
		tickInterval = 1 * time.Minute
		syncInterval = 2 * time.Hour
	)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	last := time.Now()
	lastSync := last
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			if now.Sub(last) > 2*tickInterval {
				slog.Warning("system hang detected, closing all sessions")
				s.closeAllSessions()
			} else if now.Sub(lastSync) > syncInterval {
				s.sync()
				lastSync = now
			}
			last = now
			s.maintenance()
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) CollectMetrics(w *bufio.Writer) {
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Server ===\n\n")
	s.forwarder.CollectMetrics(w)
	s.canceller.CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Stack ===\n\n")
	(&metric.Stack{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")
}
