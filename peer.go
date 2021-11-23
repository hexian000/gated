package gated

import (
	"context"
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

func (p *Peer) CallContext(ctx context.Context, serviceMethod string, args interface{}, reply interface{}) error {
	call := p.rpcClient.Go(serviceMethod, args, reply, nil)
	select {
	case <-call.Done:
		return call.Error
	case <-ctx.Done():
		_ = p.Session.Close()
		return ctx.Err()
	}
}

func (p *Peer) Dial(s *Server) error {
	if p.IsConnected() {
		return nil
	}
	ctx := util.WithTimeout(s.cfg.Timeout())
	defer util.Cancel(ctx)
	slog.Verbosef("bootstrap: setup connection to %s[%s]", p.Info.Address, p.Info.ServerName)
	tcpConn, err := s.dialer.DialContext(ctx, network, p.Info.Address)
	if err != nil {
		slog.Errorf("bootstrap: %v", err)
		return err
	}
	connId := tcpConn.RemoteAddr()
	tlsConn := tls.Client(tcpConn, s.cfg.TLSConfig(p.Info.ServerName))
	err = tlsConn.HandshakeContext(ctx)
	if err != nil {
		_ = tcpConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	s.cfg.SetConnParams(tcpConn)
	muxConn, err := yamux.Client(tlsConn, s.cfg.MuxConfig())
	if err != nil {
		_ = tlsConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	// TODO: cover in context
	rpcClientConn, err := muxConn.Open()
	if err != nil {
		_ = muxConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	rpcServerConn, err := muxConn.Accept()
	if err != nil {
		_ = muxConn.Close()
		slog.Errorf("bootstrap %v: %v", connId, err)
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
		slog.Errorf("bootstrap %v: %v", connId, err)
		return err
	}
	go func() {
		rpcServer.ServeCodec(jsonrpc.NewServerCodec(rpcServerConn))
		_ = muxConn.Close()
	}()
	go func() {
		err := s.api.Serve(muxConn)
		slog.Infof("bootstrap %v: closing %v", connId, err)
	}()
	slog.Verbosef("bootstrap %v: join cluster", connId)
	args := s.self()
	args.Timestamp = NewTimestamp()
	var reply proto.Cluster
	err = p.CallContext(ctx, "RPC.Bootstrap", args, &reply)
	if err != nil {
		_ = muxConn.Close()
		return err
	}
	slog.Verbosef("bootstrap %v: reply: %v", connId, reply)
	p.Info = reply.Self
	s.Merge(&reply)
	slog.Debugf("bootstrap %v: ok, remote peer: %s", connId, p.Info.PeerName)
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
