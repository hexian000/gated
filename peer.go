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

	server    *Server
	cfg       *Config
	resolver  Resolver
	apiServer *http.Server
	apiClient *http.Client
}

func newPeer(s *Server) *peer {
	return &peer{
		created:  time.Now(),
		server:   s,
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

func (p *peer) Open() (net.Conn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
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
		if err := p.server.dialPeer(ctx, p); err != nil {
			return nil, err
		}
	}
	conn, err := p.mux.Open()
	if err == nil {
		p.lastSeen = time.Now()
	}
	return conn, err
}

func (p *peer) newHandler() http.Handler {
	rpcServer := rpc.NewServer()
	if err := rpcServer.Register(&RPC{
		server: p.server,
		router: p.server.router,
		peer:   p,
	}); err != nil {
		panic(err)
	}
	return newAPIHandler(p.server, rpcServer)
}

func (p *peer) setup(mux *yamux.Session) {
	timeout := p.cfg.Timeout()
	p.mux = mux
	p.apiServer = &http.Server{
		Handler:           p.newHandler(),
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

func (p *peer) Serve(mux *yamux.Session) {
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.setup(mux)
	}()
	slog.Verbosef("serve %v: rpc online", mux.RemoteAddr())
	_ = p.apiServer.Serve(p.mux)
}

func (p *peer) dialMux(ctx context.Context, addr, sni string) (*yamux.Session, error) {
	slog.Verbosef("bootstrap: setup connection to %s", addr)
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

func (p *peer) Bootstrap(ctx context.Context) (*proto.Cluster, error) {
	addr, err := func() (net.Addr, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		addr, sni := p.info.Address, p.info.ServerName
		if p.mux != nil {
			_ = p.mux.Close()
		}
		mux, err := p.dialMux(ctx, addr, sni)
		if err != nil {
			return nil, err
		}
		p.setup(mux)
		go p.apiServer.Serve(p.mux)
		return mux.RemoteAddr(), nil
	}()
	if err != nil {
		return nil, err
	}

	slog.Verbosef("bootstrap %v: join cluster", addr)
	var cluster proto.Cluster
	if err := p.call(ctx, "RPC.Bootstrap", p.server.self(), &cluster); err != nil {
		return nil, err
	}
	slog.Verbosef("bootstrap %v: reply: %v", addr, cluster)
	p.info = cluster.Self
	slog.Infof("bootstrap %v: ok, remote peer: %s", addr, p.info.PeerName)
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
