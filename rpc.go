package gated

import (
	"fmt"
	"net/rpc"
	"time"

	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
)

func (s *Server) broadcast(method string, args interface{}, reply func() interface{}) <-chan *rpc.Call {
	s.mu.RLock()
	defer s.mu.RUnlock()
	chIn := make(chan *rpc.Call, 8)
	chOut := make(chan *rpc.Call, 8)
	count := 0
	for name, peer := range s.peers {
		if peer.Info.Address == "" {
			continue
		}
		if err := peer.Dial(s); err != nil {
			slog.Error("broadcast dial:", err)
			continue
		}
		slog.Verbose("broadcast:", name, method, args)
		_ = peer.rpcClient.Go(method, args, reply(), chIn)
		count++
	}
	go func() {
		defer close(chOut)
		defer close(chIn)
		for count > 0 {
			chOut <- <-chIn
			count--
		}
	}()
	return chOut
}

type RPC struct {
	server *Server
	router *Router
	peer   *Peer
}

func (r *RPC) Bootstrap(args *proto.PeerInfo, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Bootstrap: %v", args)
	r.peer.Info = args
	_ = r.server.broadcast("RPC.Update", args, func() interface{} { return &proto.None{} })
	r.server.addPeer(r.peer)
	*reply = *r.server.ClusterInfo()
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Update(args *proto.PeerInfo, reply *proto.None) error {
	slog.Verbosef("RPC.Update: %v", args)
	if r.server.Update(args) {
		_ = r.server.broadcast("RPC.Update", args, func() interface{} { return &proto.None{} })
	}
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Sync(args *proto.Cluster, reply *proto.None) error {
	slog.Verbosef("RPC.Sync: %v", args)
	r.server.Merge(args)
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Ping(args *proto.Ping, reply *proto.Ping) error {
	slog.Verbosef("RPC.Ping: %v", args)
	name := r.server.LocalPeerName()
	*reply = proto.Ping{
		Timestamp:   args.Timestamp,
		Source:      name,
		Destination: args.Destination,
	}
	if name == args.Destination {
		return nil
	}
	if args.TTL <= 0 {
		return fmt.Errorf("reply from %s: destination is unreachable", name)
	}
	args.TTL--
	if peer := r.server.getPeer(args.Destination); peer != nil && peer.Dial(r.server) == nil {
		return peer.rpcClient.Call("RPC.Ping", args, &proto.Ping{})
	}
	return fmt.Errorf("reply from %s: unknown peer name: %s", name, args.Destination)
}

func NewTimestamp() int64 {
	return time.Now().UnixMilli()
}
