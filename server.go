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
	proxy string
	saved time.Time
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
		p.info.Online = true
		go func() {
			ctx := s.canceller.WithTimeout(timeout)
			defer s.canceller.Cancel(ctx)
			if err := p.Bootstrap(ctx); err != nil {
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
	if !ok {
		return nil
	}
	if time.Since(info.saved) > s.cfg.CacheTimeout() {
		delete(s.proxyCache, destination)
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

func (s *Server) saveProxyCache(destination, proxy string) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	s.proxyCache[destination] = peerProxy{
		proxy: proxy,
		saved: time.Now(),
	}
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	if proxy := s.getProxyCache(peer); proxy != nil {
		conn, err := proxy.DialContext(ctx)
		if err != nil {
			s.deleteProxyCache(peer)
		}
		return conn, err
	}
	p := s.getPeer(peer)
	if p != nil && p.hasAddress() {
		// prefer direct
		conn, err := p.DialContext(ctx)
		if err == nil {
			return conn, nil
		}
		slog.Debugf("dial peer %s direct: %v", peer, err)
	}
	proxy, err := s.findProxy(peer)
	if err != nil {
		slog.Debug("find proxy:", err)
		return nil, err
	}
	conn, err := proxy.DialContext(ctx)
	if err == nil {
		s.saveProxyCache(peer, proxy.info.PeerName)
	}
	slog.Debugf("dial peer %s via %s", peer, proxy)
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
	for _, p := range s.getPeers() {
		info := p.PeerInfo()
		if p.isConnected() {
			p.checkNumStreams()
		} else if info.Online && info.Address != "" &&
			(!selfHasAddr || info.Timestamp == 0) {
			slog.Infof("redial %q: %q", info.PeerName, info.Address)
			go func(p *peer) {
				ctx := s.canceller.WithTimeout(s.cfg.Timeout())
				defer s.canceller.Cancel(ctx)
				if err := p.Bootstrap(ctx); err != nil {
					slog.Errorf("redial %q: %v", info.PeerName, err)
				}
			}(p)
		}
		if (selfHasAddr && info.Address != "") &&
			time.Since(p.LastUsed()) > idleTimeout {
			slog.Infof("idle timeout expired: %s", info.PeerName)
			_ = p.Close()
		}
		if time.Since(p.LastUpdate()) > peerInfoTimeout {
			slog.Infof("peer info timeout expired: %s", info.PeerName)
			s.deletePeer(info.PeerName)
			s.router.deletePeer(info.PeerName)
		}
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

	_, _ = w.WriteString("=== Peers ===\n\n")
	for name, p := range s.getPeers() {
		w.WriteString(fmt.Sprintf("%s:\n", name))
		w.WriteString(fmt.Sprintf("    HasAddress:  %v\n", p.hasAddress()))
		lastUpdatedStr := "never"
		if t := p.LastUpdate(); t != (time.Time{}) {
			lastUpdatedStr = now.Sub(t).String()
		}
		w.WriteString(fmt.Sprintf("    LastUpdated: %s\n", lastUpdatedStr))
		if p.isConnected() {
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
