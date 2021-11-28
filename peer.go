package gated

import (
	"context"
	"crypto/tls"
	"errors"
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

	server   *Server
	cfg      *Config
	resolver Resolver
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
	mux := func() *yamux.Session {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.mux
	}()
	if mux == nil {
		return nil, fmt.Errorf("peer %s is not connected", p.info.PeerName)
	}
	conn, err := p.mux.Open()
	if err == nil {
		p.lastSeen = time.Now()
	}
	return conn, err
}

func (p *peer) getSession() *yamux.Session {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mux
}

func (p *peer) DialContext(ctx context.Context) (net.Conn, error) {
	mux := p.getSession()
	if mux == nil || mux.IsClosed() {
		if err := p.Bootstrap(ctx); err != nil {
			return nil, err
		}
		mux = p.getSession()
		if mux == nil {
			return nil, errors.New("temporary failure, retry later")
		}
	}
	conn, err := mux.Open()
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

func (p *peer) serveAPI(mux *yamux.Session) {
	server := &http.Server{
		Handler:           p.newHandler(),
		ReadHeaderTimeout: p.cfg.Timeout(),
		ErrorLog:          newHTTPLogger(),
	}
	_ = server.Serve(mux)
}

func (p *peer) Serve(mux *yamux.Session) {
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.mux = mux
	}()
	slog.Verbosef("serve %v: rpc online", mux.RemoteAddr())
	p.serveAPI(mux)
}

func (p *peer) Bootstrap(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux != nil && !p.mux.IsClosed() {
		return nil
	}
	slog.Verbosef("bootstrap: setup connection to %s", p.info.Address)
	dialer := net.Dialer{}
	tcpConn, err := dialer.DialContext(ctx, network, p.info.Address)
	if err != nil {
		slog.Errorf("bootstrap: %v", err)
		return err
	}
	connId := tcpConn.RemoteAddr()
	tlsConn := tls.Client(tcpConn, p.server.cfg.TLSConfig(p.info.ServerName))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	p.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, p.server.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	go p.serveAPI(muxConn)
	p.mux = muxConn
	conn, err := p.mux.Open()
	if err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	self := p.server.self()
	slog.Verbosef("bootstrap %v: call RPC.Bootstrap: %v", connId, self)
	var cluster proto.Cluster
	if err := p.call(conn, deadline, "RPC.Bootstrap", self, &cluster); err != nil {
		return err
	}
	slog.Verbosef("bootstrap %v: reply: %v", connId, cluster)
	p.lastSeen = time.Now()
	p.info = cluster.Self
	slog.Infof("bootstrap %v: ok, remote peer: %s", connId, p.info.PeerName)
	p.server.addPeer(p)
	p.server.MergeCluster(&cluster)
	return nil
}

func (p *peer) Close() (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux != nil {
		err = p.mux.Close()
		p.mux = nil
	}
	return
}
