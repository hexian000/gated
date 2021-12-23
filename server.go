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
		statusChanged: time.Now(),
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
			if _, err := p.Bootstrap(ctx); err != nil {
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
	for call := range s.Broadcast(ctx, "RPC.Update", "", s.ClusterInfo(), reflect.TypeOf(proto.Cluster{})) {
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

func (s *Server) bootstrapAll() <-chan error {
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	ch := make(chan error, 10)
	wg := &sync.WaitGroup{}
	for _, p := range s.getPeers() {
		wg.Add(1)
		go func(p *peer) {
			defer wg.Done()
			_, err := p.Bootstrap(ctx)
			ch <- err
		}(p)
	}
	go func() {
		defer close(ch)
		wg.Wait()
	}()
	return ch
}

func (s *Server) FindProxy(peer string, tryDirect bool) (string, error) {
	for range s.bootstrapAll() {
	}
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
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
	for result := range s.Broadcast(ctx, "RPC.Ping", except, &proto.Ping{
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
	peerInfoTimeout = 8 * time.Hour
)

func (s *Server) maintenance() {
	cfg := s.cfg.Current()
	selfHasAddr := cfg.AdvertiseAddr != ""
	idleTimeout := time.Duration(cfg.Transport.IdleTimeout) * time.Second
	if count, _ := s.checkStatus(); count < 2 {
		if err := s.randomRedial(); err != nil {
			slog.Error("random redial:", err)
			if count < 1 {
				for err := range s.bootstrapAll() {
					if err == nil {
						return
					}
				}
				s.BootstrapFromConfig()
			}
		}
		return
	}
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		if !info.Online {
			s.deletePeer(name)
			s.router.deletePeer(name)
			continue
		}
		peerHasAddr := info.Address != ""
		if connected {
			p.checkNumStreams()
			if selfHasAddr && peerHasAddr &&
				time.Since(p.LastUsed()) > idleTimeout {
				slog.Infof("idle timeout expired: %q", info.PeerName)
				_ = p.Close()
			}
			continue
		}
		if time.Since(p.LastUpdate()) > peerInfoTimeout {
			slog.Infof("peer info timeout expired: %q", info.PeerName)
			p.SetOnline(false)
			continue
		}
		if !peerHasAddr && !s.router.hasProxy(info.PeerName) {
			go s.router.updateProxy(info.PeerName, false)
		}
		if peerHasAddr && !selfHasAddr {
			slog.Infof("redial %q: %q", info.PeerName, info.Address)
			go func(p *peer) {
				ctx := s.canceller.WithTimeout(s.cfg.Timeout())
				defer s.canceller.Cancel(ctx)
				if _, err := p.Bootstrap(ctx); err != nil {
					slog.Errorf("redial %q: %v", info.PeerName, err)
				}
			}(p)
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
				go s.broadcastUpdate(s.ClusterInfo())
				lastUpdate = now
				last = now
				continue
			}
			if now.Sub(lastUpdate) > updateInterval {
				slog.Debug("periodic: update")
				go s.broadcastUpdate(s.ClusterInfo())
				lastUpdate = now
			}
			last = now
			s.maintenance()
		case <-s.shutdownCh:
			return
		}
	}
}

func (s *Server) checkStatus() (int, time.Time) {
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

func (s *Server) CollectMetrics(w *bufio.Writer) {
	now := time.Now()
	writef := func(format string, a ...interface{}) {
		_, _ = w.WriteString(fmt.Sprintf(format, a...))
	}
	func() {
		count, statusChanged := s.checkStatus()
		status := "Outage"
		if count > 0 {
			status = "Operating Normally"
		}
		writef("Status: %s, %v\n\n", status, time.Since(statusChanged))
	}()
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Peers ===\n")
	cacheTimeout := s.cfg.CacheTimeout()
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		if info.Online {
			writef("\nPeer %q\n", name)
		} else {
			writef("\nPeer %q (offline)\n", name)
		}
		if info.Address != "" {
			writef("    %-16s  %q\n", "Address:", info.Address)
		} else {
			writef("    %-16s  %s\n", "Address:", "(unreachable)")
		}
		lastUsed := p.LastUsed()
		status := "disconnected"
		if connected {
			numStreams := 0
			if mux := p.MuxSession(); mux != nil && !mux.IsClosed() {
				numStreams = mux.NumStreams()
			}
			if numStreams > 0 {
				status = fmt.Sprintf("%d streams", numStreams)
			} else if info.Address == "" {
				status = "linger"
			} else {
				status = "idle, " + now.Sub(lastUsed).String()
			}
		}
		writef("    %-16s  %s\n", "Status:", status)
		writef("    %-16s  %s\n", "LastUsed:", formatAgo(now, lastUsed))
		if proxy := s.router.getProxy(name, cacheTimeout); proxy == "" {
			writef("    %-16s  %s\n", "Proxy:", "(direct)")
		} else {
			writef("    %-16s  %q\n", "Proxy:", proxy)
		}
		writef("    %-16s  %s\n", "Created:", formatAgo(now, p.Created()))
		writef("    %-16s  %s\n", "LastUpdated:", formatAgo(now, p.LastUpdate()))
		if meter := p.GetMeter(); meter != nil {
			read, written := meter.Count()
			writef("    %-16s  %s / %s\n", "Bandwidth(U/D):", formatIEC(written), formatIEC(read))
		}
		writef("    %-16s  %q\n", "Version:", info.Version)
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
