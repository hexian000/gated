package gated

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
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

func (s *Server) FindProxy(peer string) (string, error) {
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	type proxy struct {
		name string
		ttl  int
		rtt  time.Duration
	}
	list := make([]proxy, 0)
	start := time.Now()
	for result := range s.Broadcast(ctx, "RPC.Ping", &proto.Ping{
		Source:      s.LocalPeerName(),
		Destination: peer,
		TTL:         2,
	}, reflect.TypeOf(proto.Ping{})) {
		if result.err != nil {
			continue
		}
		list = append(list, proxy{
			name: result.from.Name(),
			ttl:  result.reply.(*proto.Ping).TTL,
			rtt:  time.Since(start),
		})
	}
	if len(list) < 1 {
		return "", fmt.Errorf("find proxy: %s is unreachable", peer)
	} else if len(list) > 1 {
		sort.SliceStable(list, func(i int, j int) bool {
			if list[i].ttl == list[j].ttl {
				return list[i].rtt < list[j].rtt
			}
			return list[i].ttl > list[j].ttl
		})
	}
	return list[0].name, nil
}

func (s *Server) DialPeerContext(ctx context.Context, peer string) (net.Conn, error) {
	p := s.getPeer(peer)
	if p == nil {
		return nil, fmt.Errorf("unknown peer: %q", peer)
	}
	info, connected := p.PeerInfo()
	if !connected && info.Address == "" {
		return nil, fmt.Errorf("peer %q is unreachable", peer)
	}
	slog.Debugf("dial peer %q direct", peer)
	return p.DialContext(ctx)
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
	if connectedCount < 1 {
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

	writef := func(format string, a ...interface{}) {
		w.WriteString(fmt.Sprintf(format, a...))
	}
	_, _ = w.WriteString("=== Peers ===\n")
	cacheTimeout := s.cfg.CacheTimeout()
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		writef("\nPeer %q\n", name)
		writef("    %-16s  %q\n", "Address:", info.Address)
		if connected {
			writef("    %-16s  %v\n", "Connected:", connected)
		} else {
			writef("    %-16s  %s\n", "LastConnected:", formatSince(now, p.LastUsed()))
		}
		writef("    %-16s  %v\n", "Online:", info.Online)
		if proxy := s.router.getProxy(name, cacheTimeout); proxy == "" {
			writef("    %-16s  %s\n", "Proxy:", "(direct)")
		} else {
			writef("    %-16s  %q\n", "Proxy:", proxy)
		}
		writef("    %-16s  %s\n", "Created:", formatSince(now, p.Created()))
		writef("    %-16s  %s\n", "LastUpdated:", formatSince(now, p.LastUpdate()))
		if meter := p.meter; meter != nil {
			read, written := meter.Count()
			writef("    %-16s  %v / %v\n", "Bandwidth:", formatIEC(read), formatIEC(written))
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
