package gated

import (
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

	operational   bool
	statusChanged time.Time

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
	s.router = NewRouter(s)
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
	wg := sync.WaitGroup{}
	cfg := s.cfg.Current()
	for _, server := range cfg.Servers {
		p := newPeer(s)
		p.info.Address = server.Address
		p.info.ServerName = server.ServerName
		p.info.Online = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Bootstrap()
		}()
	}
	wg.Wait()
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
	if cfg.HTTPListen != "" {
		slog.Info("http listen:", cfg.HTTPListen)
		l, err := net.Listen(network, cfg.HTTPListen)
		if err != nil {
			slog.Error("listen:", err)
			return err
		}
		s.httpListener = l
	}
	s.BootstrapFromConfig()
	if l := s.httpListener; l != nil {
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
	for call := range s.Broadcast(ctx, "RPC.Update", "", false, s.ClusterInfo(), reflect.TypeOf(proto.Cluster{})) {
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

func (s *Server) FindProxy(peer string, tryDirect, fast bool) (string, error) {
	ctx := s.canceller.WithTimeout(pingTimeout)
	defer s.canceller.Cancel(ctx)
	type proxy struct {
		name string
		ttl  int
		rtt  time.Duration
	}
	list := make([]proxy, 0)
	except := peer
	if tryDirect {
		except = ""
	}
	start := time.Now()
	for result := range s.Broadcast(ctx, "RPC.Lookup", except, !fast, &proto.Lookup{
		Source:      s.LocalPeerName(),
		Destination: peer,
		TTL:         2,
		Fast:        fast,
	}, reflect.TypeOf(proto.Lookup{})) {
		if result.err != nil {
			continue
		}
		list = append(list, proxy{
			name: result.from.Name(),
			ttl:  result.reply.(*proto.Lookup).TTL,
			rtt:  time.Since(start),
		})
	}
	if len(list) < 1 {
		return "", fmt.Errorf("%q is unreachable", peer)
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
	muxConn, err := yamux.Server(tlsConn, s.cfg.MuxConfig(connId.String()))
	if err != nil {
		slog.Errorf("serve %v: %v", connId, err)
		_ = tlsConn.Close()
		return
	}
	p := newPeer(s)
	p.Bind(muxConn, meteredConn)
	slog.Infof("serve %v: rpc online, setup: %v", connId, time.Since(setupBegin))
}

func (s *Server) OnPeerLost(p *peer) {
	info, _ := p.PeerInfo()
	slog.Infof("peer lost: %q", info.PeerName)
	cfg := s.cfg.Current()
	idleEnabled := cfg.Transport.IdleTimeout > 0
	idleTimeout := time.Duration(cfg.Transport.IdleTimeout) * time.Second
	if idleEnabled && time.Since(p.LastUsed()) > idleTimeout {
		return
	}
	s.router.UpdateProxy(info.PeerName)
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
	idleEnabled := cfg.Transport.IdleTimeout > 0
	idleTimeout := time.Duration(cfg.Transport.IdleTimeout) * time.Second
	if count, _ := s.updateStatus(); count < 2 {
		idleEnabled = false
		if !s.randomRedial() && count < 1 {
			s.BootstrapFromConfig()
		}
	}
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		if !info.Online {
			lastUpdate := p.LastUpdate()
			if time.Since(lastUpdate) > peerInfoTimeout {
				slog.Infof("peer info timeout expired: %q", info.PeerName)
				s.deletePeer(name)
				s.router.deletePeer(name)
			}
		}
		peerHasAddr := info.Address != ""
		if connected {
			p.Seen(false)
			lastUsed := p.LastUsed()
			if idleEnabled && selfHasAddr && peerHasAddr &&
				name != cfg.Routes.Default &&
				lastUsed != (time.Time{}) && time.Since(p.LastUsed()) > idleTimeout {
				slog.Infof("idle timeout expired: %q", info.PeerName)
				_ = p.Close()
			}
			continue
		}
		if peerHasAddr && !selfHasAddr {
			slog.Infof("always redial %q: %q", info.PeerName, info.Address)
			p.Bootstrap()
		}
	}
}

func (s *Server) watchdog() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	lastTick := time.Now()
	lastUpdate := lastTick
	for {
		select {
		case <-ticker.C:
			now := time.Now()
			tickInterval := now.Sub(lastTick)
			lastTick = now
			if tickInterval > 2*tickInterval {
				slog.Warning("system hang detected, tick time:", now.Sub(lastTick))
				s.closeAllSessions()
				go s.broadcastUpdate(s.ClusterInfo())
				lastUpdate = now
				continue
			}
			if now.Sub(lastUpdate) > updateInterval {
				slog.Debug("periodic: update")
				go s.broadcastUpdate(s.ClusterInfo())
				lastUpdate = now
			}
			s.maintenance()
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) updateStatus() (int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, p := range s.peers {
		if info, connected := p.PeerInfo(); info.Online && connected {
			count++
		}
	}
	operational := count > 0
	if s.operational != operational {
		s.operational = operational
		s.statusChanged = time.Now()
	}
	return count, s.statusChanged
}

func (s *Server) getStatus() (bool, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.operational, s.statusChanged
}
