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

	bootstrapMu sync.RWMutex

	server   *Server
	cfg      *Config
	resolver Resolver
}

func newPeer(s *Server) *peer {
	return &peer{
		server:   s,
		cfg:      s.cfg,
		resolver: s.router,
	}
}

func (p *peer) Name() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info.PeerName
}

func (p *peer) IsConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mux != nil && !p.mux.IsClosed()
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

func (p *peer) Seen(used bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil || p.mux.IsClosed() {
		return
	}
	now := time.Now()
	p.lastSeen = now
	if used || p.mux.NumStreams() > 0 {
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

func (p *peer) Bind(mux *yamux.Session, meter *util.MeteredConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux != nil {
		_ = p.mux.Close()
	}
	if p.meter != nil {
		_ = p.meter.Close()
	}
	p.mux, p.meter = mux, meter
	go p.serve(mux)
}

func (p *peer) UpdateInfo(info *proto.PeerInfo) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastUpdate = time.Now()
	if p.info.Timestamp < info.Timestamp {
		slog.Debug("peer info update:", info)
		p.info = *info
		return true
	}
	return false
}

func (p *peer) DialContext(ctx context.Context) (conn net.Conn, err error) {
	defer p.Seen(err == nil)
	mux := p.MuxSession()
	if mux == nil || mux.IsClosed() {
		p.Bootstrap()
		mux = p.MuxSession()
		if mux == nil || mux.IsClosed() {
			return nil, fmt.Errorf("peer %q temporarily not reachable", p.Name())
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

func (p *peer) serve(l net.Listener) {
	server := &http.Server{
		Handler:           p.newHandler(),
		ReadHeaderTimeout: p.cfg.Timeout(),
		ErrorLog:          newHTTPLogger(),
	}
	_ = server.Serve(l)
}

func (p *peer) Bootstrap() {
	info, _ := p.PeerInfo()
	if info.Address == "" {
		slog.Errorf("bootstrap: peer %q has no address", info.PeerName)
		return
	}
	if !info.Online {
		slog.Errorf("bootstrap: peer %q is offline", info.PeerName)
		return
	}
	p.bootstrapMu.Lock()
	defer p.bootstrapMu.Unlock()
	if mux := p.MuxSession(); mux != nil && !mux.IsClosed() {
		return
	}
	ctx := p.server.canceller.WithTimeout(p.cfg.Timeout())
	defer p.server.canceller.Cancel(ctx)
	slog.Infof("bootstrap: setup connection to %s", info.Address)
	setupBegin := time.Now()
	dialer := net.Dialer{}
	tcpConn, err := dialer.DialContext(ctx, network, info.Address)
	if err != nil {
		slog.Errorf("bootstrap: %v", err)
		return
	}
	connId := tcpConn.RemoteAddr()
	meteredConn := util.Meter(tcpConn)
	tlsConn := tls.Client(meteredConn, p.server.cfg.TLSConfig(info.ServerName))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return
	}
	p.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, p.server.cfg.MuxConfig(connId.String()))
	if err != nil {
		_ = tlsConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return
	}
	conn, err := muxConn.Open()
	if err != nil {
		slog.Errorf("bootstrap %v: mux open: %v", connId, err)
		_ = muxConn.Close()
		return
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	bootstrapMsg := p.server.ClusterInfo()
	slog.Verbosef("bootstrap %v: call RPC.Bootstrap: %v", connId, bootstrapMsg)
	var cluster proto.Cluster
	if err := p.call(conn, deadline, "RPC.Bootstrap", bootstrapMsg, &cluster); err != nil {
		slog.Errorf("bootstrap %v: RPC.Bootstrap: %v", connId, err)
		_ = muxConn.Close()
		return
	}
	slog.Verbosef("bootstrap %v: reply: %v", connId, cluster)
	p.UpdateInfo(&cluster.Self)
	p.Seen(true)
	p.Bind(muxConn, meteredConn)
	slog.Infof("bootstrap %v: ok, remote name: %q, setup: %v", connId, cluster.Self.PeerName, time.Since(setupBegin))
	p.server.addPeer(p)
	p.server.router.setProxy(cluster.Self.PeerName, "")
	p.server.MergeCluster(&cluster)
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
