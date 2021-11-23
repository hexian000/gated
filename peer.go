package gated

import (
	"crypto/tls"
	"net/rpc"
	"net/rpc/jsonrpc"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

type Peer struct {
	Session   *yamux.Session
	Info      proto.PeerInfo
	Created   time.Time
	LastSeen  time.Time
	rpcServer *rpc.Server
	rpcClient *rpc.Client
}

func (p *Peer) IsConnected() bool {
	return p.Session != nil && !p.Session.IsClosed()
}

func (p *Peer) Dial(s *Server) error {
	if p.IsConnected() {
		return nil
	}
	ctx := util.WithTimeout(s.cfg.Timeout())
	defer util.Cancel(ctx)
	slog.Verbosef("bootstrap %s: setup connection to %s", p.Info.PeerName, p.Info.Address)
	tcpConn, err := s.dialer.DialContext(ctx, network, p.Info.Address)
	if err != nil {
		return err
	}
	tlsConn := tls.Client(tcpConn, s.cfg.TLSConfig(p.Info.ServerName))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		return err
	}
	s.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, s.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		return err
	}
	// TODO: cover in context
	rpcClientConn, err := muxConn.Open()
	if err != nil {
		return err
	}
	rpcServerConn, err := muxConn.Accept()
	if err != nil {
		return err
	}
	rpcServer := rpc.NewServer()
	rpcClient := jsonrpc.NewClient(rpcClientConn)
	now := time.Now()
	p.Session = muxConn
	p.Created = now
	p.LastSeen = now
	p.rpcServer = rpcServer
	p.rpcClient = rpcClient
	if err := rpcServer.Register(&RPC{
		server: s,
		router: s.router,
		peer:   p,
	}); err != nil {
		_ = muxConn.Close()
		slog.Errorf("bootstrap %s: %v", p.Info.PeerName, err)
		return err
	}
	go func() {
		rpcServer.ServeCodec(jsonrpc.NewServerCodec(rpcServerConn))
		_ = muxConn.Close()
	}()
	go func() {
		err := s.api.Serve(muxConn)
		slog.Info("session closing:", err)
	}()
	slog.Verbosef("bootstrap %s: join cluster", p.Info.PeerName)
	args := s.self()
	args.Timestamp = NewTimestamp()
	var reply proto.Cluster
	err = rpcClient.Call("RPC.Bootstrap", args, &reply)
	if err != nil {
		return err
	}
	slog.Verbosef("bootstrap %s: reply: %v", p.Info.PeerName, reply)
	p.Info = reply.Self
	s.Merge(&reply)
	slog.Debugf("bootstrap %s: ok", p.Info.PeerName)
	s.print()
	s.router.print()
	return nil
}

func (p *Peer) Close() error {
	if p.Session != nil {
		return p.Session.Close()
	}
	return nil
}
