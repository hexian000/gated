package gated

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/rpc"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/slog"
)

type peer struct {
	mu       sync.RWMutex
	mux      *yamux.Session
	info     proto.PeerInfo
	created  time.Time
	lastSeen time.Time

	cfg       *Config
	resolver  Resolver
	apiServer *http.Server
	apiClient *http.Client
}

func newPeer(s *Server) *peer {
	return &peer{
		created:  time.Now(),
		cfg:      s.cfg,
		resolver: s.router,
	}
}

func (p *peer) isReachable() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info.Address != ""
}

func (p *peer) isConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mux != nil && !p.mux.IsClosed()
}

func (p *peer) checkNumStreams() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil || p.mux.IsClosed() {
		return
	}
	if p.mux.NumStreams() > 0 {
		p.lastSeen = time.Now()
	}
}

func (p *peer) dialMux(ctx context.Context, addr, sni string) (*yamux.Session, error) {
	slog.Verbosef("bootstrap: setup connection to %s[%s]", addr, sni)
	dialer := net.Dialer{}
	tcpConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		slog.Errorf("bootstrap: %v", err)
		return nil, err
	}
	connId := tcpConn.RemoteAddr()
	tlsConn := tls.Client(tcpConn, p.cfg.TLSConfig(sni))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return nil, err
	}
	p.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, p.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return nil, err
	}
	return muxConn, nil
}

func (p *peer) Open() (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil {
		return nil, fmt.Errorf("peer %s is not connected", p.info.PeerName)
	}
	conn, err := p.mux.Open()
	if err == nil {
		p.lastSeen = time.Now()
	}
	return conn, err
}

func (p *peer) DialContext(ctx context.Context) (net.Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil || p.mux.IsClosed() {
		if p.info.Address == "" {
			return nil, fmt.Errorf("peer %s didn't advertise an address", p.info.PeerName)
		}
		mux, err := p.dialMux(ctx, p.info.Address, p.info.ServerName)
		if err != nil {
			return nil, err
		}
		p.mux = mux
	}
	conn, err := p.mux.Open()
	if err == nil {
		p.lastSeen = time.Now()
	}
	return conn, err
}

func (p *peer) newHandler(s *Server) http.Handler {
	rpcServer := rpc.NewServer()
	if err := rpcServer.Register(&RPC{
		server: s,
		router: s.router,
		peer:   p,
	}); err != nil {
		panic(err)
	}
	return newAPIHandler(s, rpcServer)
}

func (p *peer) setup(s *Server, mux *yamux.Session) {
	timeout := p.cfg.Timeout()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mux = mux
	p.apiServer = &http.Server{
		Handler:           p.newHandler(s),
		ReadHeaderTimeout: timeout,
		ErrorLog:          newHTTPLogger(),
	}
	p.apiClient = &http.Client{
		Transport: &http.Transport{
			Proxy: p.resolver.Proxy,
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				return p.DialContext(ctx)
			},
			DisableKeepAlives: true,
		},
		Timeout: timeout,
	}
}

func (p *peer) Serve(s *Server, mux *yamux.Session) {
	p.setup(s, mux)
	_ = p.apiServer.Serve(p.mux)
}

func (p *peer) Bootstrap(ctx context.Context, s *Server) (*proto.Cluster, error) {
	var addr, sni string
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		addr, sni = p.info.Address, p.info.ServerName
		if p.mux != nil {
			_ = p.mux.Close()
		}
	}()
	mux, err := p.dialMux(ctx, addr, sni)
	if err != nil {
		return nil, err
	}
	p.setup(s, mux)
	go p.apiServer.Serve(p.mux)
	connId := p.mux.RemoteAddr()
	slog.Verbosef("bootstrap %v: join cluster", connId)
	var cluster proto.Cluster
	if err := p.call(ctx, "RPC.Bootstrap", s.self(), &cluster); err != nil {
		return nil, err
	}
	slog.Verbosef("bootstrap %v: reply: %v", connId, cluster)
	p.info = cluster.Self
	slog.Debugf("bootstrap %v: ok, remote peer: %s", connId, p.info.PeerName)
	return &cluster, nil
}

func (p *peer) Close() (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux != nil {
		err = p.mux.Close()
		p.mux = nil
	}
	p.apiServer = nil
	p.apiClient = nil
	return
}
