package gated

import (
	"context"
	"fmt"
	"net/rpc"
	"reflect"
	"time"

	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

func (s *Server) broadcast(ctx context.Context, method string, args interface{}, replyType reflect.Type) <-chan *rpc.Call {
	s.mu.RLock()
	defer s.mu.RUnlock()
	chIn := make(chan *rpc.Call, 10)
	chOut := make(chan *rpc.Call, 10)
	count := 0
	for name, peer := range s.peers {
		if !peer.IsConnected() {
			continue
		}
		slog.Verbose("broadcast:", name, method, args)
		_ = peer.rpcClient.Go(method, args, reflect.New(replyType).Interface(), chIn)
		count++
	}
	go func() {
		defer close(chOut)
		for count > 0 {
			select {
			case call := <-chIn:
				chOut <- call
			case <-ctx.Done():
				return
			}
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
	r.peer.Info = *args
	go func() {
		ctx := util.WithTimeout(r.server.cfg.Timeout())
		defer util.Cancel(ctx)
		for range r.server.broadcast(ctx, "RPC.Update", args, reflect.TypeOf(proto.None{})) {
		}
	}()
	r.server.addPeer(r.peer)
	*reply = *r.server.ClusterInfo()
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Update(args *proto.PeerInfo, reply *proto.None) error {
	slog.Verbosef("RPC.Update: %v", args)
	if args.PeerName == r.server.LocalPeerName() {
		return nil
	}
	if r.server.Update(*args) {
		go func() {
			ctx := util.WithTimeout(r.server.cfg.Timeout())
			defer util.Cancel(ctx)
			for range r.server.broadcast(ctx, "RPC.Update", args, reflect.TypeOf(proto.None{})) {
			}
		}()
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
