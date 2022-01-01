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
	"github.com/hexian000/gated/util"
)

type peer struct {
	mu    sync.RWMutex
	mux   *yamux.Session
	meter *util.MeteredConn
	info  proto.PeerInfo

	lastSeen   time.Time
	lastUsed   time.Time
	lastUpdate time.Time

	bootstrapCh chan struct{}

	server   *Server
	cfg      *Config
	resolver Resolver
}

func newPeer(s *Server) *peer {
	return &peer{
		bootstrapCh: make(chan struct{}, 1),
		server:      s,
		cfg:         s.cfg,
		resolver:    s.router,
	}
}

func (p *peer) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info.PeerName
}

func (p *peer) PeerInfo() (info proto.PeerInfo, connected bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info, p.mux != nil && !p.mux.IsClosed()
}

func (p *peer) GetMeter() *util.MeteredConn {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.meter
}

func (p *peer) updateStatus() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil || p.mux.IsClosed() {
		return
	}
	now := time.Now()
	p.lastSeen = now
	if p.mux.NumStreams() > 0 {
		p.lastUsed = now
	}
}

func (p *peer) LastSeen() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastSeen
}

func (p *peer) LastUsed() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastUsed
}

func (p *peer) MuxSession() *yamux.Session {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mux
}

func (p *peer) LastUpdate() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastUpdate
}

func (p *peer) UpdateInfo(info *proto.PeerInfo) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.info.Timestamp < info.Timestamp {
		slog.Debug("peer info update:", info)
		p.info = *info
		p.lastUpdate = time.Now()
		return true
	}
	return false
}

func (p *peer) DialContext(ctx context.Context) (net.Conn, error) {
	defer p.updateStatus()
	mux := p.MuxSession()
	if mux == nil || mux.IsClosed() {
		var err error
		mux, err = p.Bootstrap(ctx)
		if err != nil {
			return nil, err
		}
	}
	return mux.Open()
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

func (p *peer) Bootstrap(ctx context.Context) (*yamux.Session, error) {
	info, connected := p.PeerInfo()
	if info.Address == "" {
		return nil, fmt.Errorf("peer %q has no address", info.PeerName)
	}
	if !info.Online {
		return nil, fmt.Errorf("peer %q is offline", info.PeerName)
	}
	p.bootstrapCh <- struct{}{}
	defer func() {
		<-p.bootstrapCh
	}()
	if connected {
		return p.MuxSession(), nil
	}

	slog.Verbosef("bootstrap: setup connection to %s", info.Address)
	setupBegin := time.Now()
	dialer := net.Dialer{}
	tcpConn, err := dialer.DialContext(ctx, network, info.Address)
	if err != nil {
		slog.Errorf("bootstrap: %v", err)
		return nil, err
	}
	connId := tcpConn.RemoteAddr()
	meteredConn := util.Meter(tcpConn)
	tlsConn := tls.Client(meteredConn, p.server.cfg.TLSConfig(info.ServerName))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return nil, err
	}
	p.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, p.server.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return nil, err
	}
	conn, err := muxConn.Open()
	if err != nil {
		_ = muxConn.Close()
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	bootstrapMsg := p.server.ClusterInfo()
	slog.Verbosef("bootstrap %v: call RPC.Bootstrap: %v", connId, bootstrapMsg)
	var cluster proto.Cluster
	if err := p.call(conn, deadline, "RPC.Bootstrap", bootstrapMsg, &cluster); err != nil {
		_ = muxConn.Close()
		return nil, err
	}
	slog.Verbosef("bootstrap %v: reply: %v", connId, cluster)
	p.server.router.setProxy(info.PeerName, "")
	go p.serveAPI(muxConn)
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.mux = muxConn
		p.meter = meteredConn
		p.info = cluster.Self
	}()
	slog.Infof("dial %v: ok, remote name: %q, setup: %v", connId, info.PeerName, time.Since(setupBegin))
	p.server.addPeer(p)
	p.server.MergeCluster(&cluster)
	return muxConn, nil
}

func (p *peer) GoAway() (err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.mux != nil {
		err = p.mux.GoAway()
	}
	return
}

func (p *peer) Close() (err error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.mux != nil {
		err = p.mux.Close()
		p.mux = nil
	}
	return
}
