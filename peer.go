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
	Info      *proto.PeerInfo
	Created   time.Time
	LastSeen  time.Time
	rpcServer *rpc.Server
	rpcClient *rpc.Client
	rproxy    bool
}

func (p *Peer) IsConnected() bool {
	return p.Info != nil && p.Session != nil && !p.Session.IsClosed()
}

func (p *Peer) Dial(s *Server) error {
	ctx := util.WithTimeout(s.cfg.Timeout())
	defer util.Cancel(ctx)
	slog.Verbose("bootstrap: setup connection")
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
		slog.Error("bootstrap:", err)
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
	slog.Verbose("bootstrap: rpc roundtripping")
	args := s.self.Clone()
	args.Timestamp = NewTimestamp()
	var reply proto.Cluster
	err = rpcClient.Call("RPC.Bootstrap", args, &reply)
	if err != nil {
		return err
	}
	slog.Debugf("bootstrap: reply: %v", reply)
	peerInfo := reply.Self
	p.Info = &peerInfo
	s.router.merge(reply.Routes)
	slog.Verbose("bootstrap: ok")
	return nil
}
