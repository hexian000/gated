package gated

import (
	"time"

	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
)

func (s *Server) broadcast(method string, args interface{}) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, peer := range s.peers {
		if !peer.IsConnected() {
			continue
		}
		var reply proto.None
		slog.Verbose("broadcast:", name, method, args)
		_ = peer.rpcClient.Go(method, args, &reply, s.rpcCall)
	}
}

type RPC struct {
	server *Server
	router *Router
	peer   *Peer
}

func (r *RPC) Bootstrap(args *proto.PeerInfo, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Bootstrap: %v", args)
	r.peer.Info = args
	if args.RProxy == r.server.self.PeerName {
		r.peer.rproxy = true
	}
	r.server.broadcast("RPC.Add", args)
	r.server.addPeer(r.peer)
	*reply = *r.server.ClusterInfo()
	return nil
}

func (r *RPC) Add(args *proto.PeerInfo, reply *proto.None) error {
	slog.Verbosef("RPC.Add: %v", args)
	r.server.Update(args)
	return nil
}

func (r *RPC) Update(args *proto.Cluster, reply *proto.None) error {
	r.server.Merge(args)
	return nil
}

func NewTimestamp() int64 {
	return time.Now().UnixMilli()
}
