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

	proxyMu    sync.Mutex
	proxyCache map[string]peerProxy

	shutdownCh chan struct{}
}

type peerProxy struct {
	ready   bool
	proxy   string
	updated time.Time
}

func NewServer(cfg *Config) *Server {
	timeout := cfg.Timeout()
	s := &Server{
		cfg:        cfg,
		peers:      make(map[string]*peer),
		forwarder:  proxy.NewForwarder(),
		canceller:  util.NewCanceller(),
		proxyCache: make(map[string]peerProxy),
		shutdownCh: make(chan struct{}),
		httpServer: &http.Server{
			ReadHeaderTimeout: timeout,
			ErrorLog:          newHTTPLogger(),
		},
	}
	s.handler = newAPIHandler(s, nil)
	s.router = NewRouter(cfg.main.Domain, cfg.main.Routes.Default, s, cfg.main.Hosts)
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

func (s *Server) BootstrapFromConfig() {
	cfg := s.cfg.Current()
	timeout := time.Duration(cfg.Transport.Timeout) * time.Second
	for _, server := range cfg.Servers {
		p := newPeer(s)
		p.info.Address = server.Address
		p.info.ServerName = server.ServerName
		p.info.Online = true
		go func() {
			ctx := s.canceller.WithTimeout(timeout)
			defer s.canceller.Cancel(ctx)
			if err := p.Bootstrap(ctx); err != nil {
				slog.Error("start:", err)
			}
		}()
	}
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
	s.BootstrapFromConfig()
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

func (s *Server) Shutdown(ctx context.Context) error {
	close(s.shutdownCh)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.httpListener != nil {
			_ = s.httpListener.Close()
			s.httpListener = nil
		}
		if s.tlsListener != nil {
			_ = s.tlsListener.Close()
			s.tlsListener = nil
		}
	}()
	for _, p := range s.getPeers() {
		_ = p.GoAway()
	}
	s.canceller.CancelAll()
	for call := range s.Broadcast(ctx, "RPC.Update", s.ClusterInfo(), reflect.TypeOf(proto.Cluster{})) {
		if call.err != nil {
			slog.Debugf("call RPC.Update: %v", call.err)
			continue
		}
	}
	s.forwarder.Shutdown()
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for name, p := range s.peers {
			_ = p.Close()
			delete(s.peers, name)
		}
	}()
	return nil
}

func (s *Server) findProxy(peer string) (*peer, error) {
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
		return result.from, nil
	}
	return nil, fmt.Errorf("ping: %s is unreachable", peer)
}

func (s *Server) getProxyCache(destination string) *peer {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	info, ok := s.proxyCache[destination]
	if !ok || !info.ready {
		return nil
	}
	if time.Since(info.updated) > s.cfg.CacheTimeout() {
		go s.updateProxyTo(destination)
		return nil
	}
	p := s.getPeer(info.proxy)
	if p == nil {
		delete(s.proxyCache, destination)
		return nil
	}
	return p
}

func (s *Server) deleteProxyCache(destination string) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	delete(s.proxyCache, destination)
}

func (s *Server) createProxyCache(destination string) bool {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	info, ok := s.proxyCache[destination]
	if ok && !info.ready {
		return false
	}
	s.proxyCache[destination] = peerProxy{ready: false}
	return true
}

func (s *Server) setProxyCache(destination, proxy string) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	s.proxyCache[destination] = peerProxy{
		ready:   true,
		proxy:   proxy,
		updated: time.Now(),
	}
}

func (s *Server) updateProxyTo(destination string) {
	if ok := s.createProxyCache(destination); !ok {
		return
	}
	var proxy *peer
	defer func() {
		if proxy == nil {
			s.deleteProxyCache(destination)
			return
		}
		s.setProxyCache(destination, proxy.Name())
	}()
	var err error
	proxy, err = s.findProxy(destination)
	if err != nil {
		slog.Error("find proxy:", err)
	}
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	if proxy := s.getProxyCache(peer); proxy != nil {
		slog.Debugf("dial peer %q via %q", proxy.Name())
		conn, err := proxy.DialContext(ctx)
		if err != nil {
			s.deleteProxyCache(peer)
		}
		return conn, err
	}
	p := s.getPeer(peer)
	if p == nil {
		return nil, fmt.Errorf("unknown peer: %q", peer)
	}
	info, connected := p.PeerInfo()
	if !connected && info.Address == "" {
		return nil, fmt.Errorf("peer %q is unreachable", peer)
	}
	slog.Debugf("dial peer %q direct", peer)
	conn, err := p.DialContext(ctx)
	if err != nil {
		slog.Errorf("dial peer %q direct failed: %v", peer, err)
		go s.updateProxyTo(peer)
	}
	return conn, err
}

func (s *Server) serve(tcpConn net.Conn) {
	connId := tcpConn.RemoteAddr()
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	slog.Verbosef("serve %v: setup connection", connId)
	setupBegin := time.Now()
	meteredConn := util.Meter(tcpConn)
	tlsConn := tls.Server(meteredConn, s.cfg.tls)
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
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.mux = muxConn
		p.meter = meteredConn
	}()
	slog.Infof("serve %v: rpc online, setup: %v", connId, time.Since(setupBegin))
	p.serveAPI(muxConn)
}

func (s *Server) closeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		_ = p.Close()
	}
}

const (
	tickInterval    = 1 * time.Minute
	updateInterval  = 2 * time.Hour
	peerInfoTimeout = 24 * time.Hour
)

func (s *Server) maintenance() {
	cfg := s.cfg.Current()
	selfHasAddr := cfg.AdvertiseAddr != ""
	idleTimeout := time.Duration(cfg.Transport.IdleTimeout) * time.Second
	connectedCount := 0
	for _, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		if connected {
			p.checkNumStreams()
			if (selfHasAddr && info.Address != "") &&
				time.Since(p.LastUsed()) > idleTimeout {
				slog.Infof("idle timeout expired: %q", info.PeerName)
				_ = p.Close()
			} else {
				connectedCount++
			}
		} else if !selfHasAddr && info.Online && info.Address != "" {
			slog.Infof("redial %q: %q", info.PeerName, info.Address)
			go func(p *peer) {
				ctx := s.canceller.WithTimeout(s.cfg.Timeout())
				defer s.canceller.Cancel(ctx)
				if err := p.Bootstrap(ctx); err != nil {
					slog.Errorf("redial %q: %v", info.PeerName, err)
				}
			}(p)
		} else if time.Since(p.LastUpdate()) > peerInfoTimeout {
			slog.Infof("peer info timeout expired: %q", info.PeerName)
			p.SetOnline(false)
		}
	}
	if !selfHasAddr && connectedCount < 1 {
		s.BootstrapFromConfig()
	}
}

func (s *Server) watchdog() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	last := time.Now()
	lastUpdate := last
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			if now.Sub(last) > 2*tickInterval {
				slog.Warning("system hang detected, tick time:", now.Sub(last))
				s.closeAllSessions()
				go s.broatcastUpdate(s.ClusterInfo())
				lastUpdate = now
				last = now
				continue
			}
			if now.Sub(lastUpdate) > updateInterval {
				slog.Debug("periodic: update")
				go s.broatcastUpdate(s.ClusterInfo())
				lastUpdate = now
			}
			last = now
			s.maintenance()
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) CollectMetrics(w *bufio.Writer) {
	now := time.Now()
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Peers ===\n")
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		w.WriteString(fmt.Sprintf("\nPeer %q\n", name))
		w.WriteString(fmt.Sprintf("    Address:     %q\n", info.Address))
		w.WriteString(fmt.Sprintf("    Online:      %v\n", info.Online))
		lastUpdatedStr := "never"
		if t := p.LastUpdate(); t != (time.Time{}) {
			lastUpdatedStr = now.Sub(t).String()
		}
		w.WriteString(fmt.Sprintf("    LastUpdated: %s\n", lastUpdatedStr))
		if connected {
			created := p.Created()
			w.WriteString(fmt.Sprintf("    Connected:   %v (since %v)\n", now.Sub(created).String(), created))
			lastUsedStr := "never"
			if t := p.LastUsed(); t != (time.Time{}) {
				lastUsedStr = now.Sub(t).String()
			}
			w.WriteString(fmt.Sprintf("    LastUsed:    %s\n", lastUsedStr))
			read, written := p.meter.Count()
			w.WriteString(fmt.Sprintf("    Bandwidth:   %v / %v\n", read, written))
		}
	}
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Server ===\n\n")
	s.forwarder.CollectMetrics(w)
	s.canceller.CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Stack ===\n\n")
	(&metric.Stack{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")
}
