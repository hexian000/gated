package gated

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/rpc"
	"net/url"
	"reflect"
	"sync"

	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/slog"
)

func (p *peer) call(ctx context.Context, method string, args interface{}, reply interface{}) error {
	c, err := p.DialContext(ctx)
	if err != nil {
		slog.Debug("rpc call:", err)
		return err
	}
	host := p.cfg.GetFQDN(apiHost)
	req := &http.Request{
		Method: http.MethodConnect,
		URL: &url.URL{
			Host: host,
			Path: PathRPC,
		},
		Header: map[string][]string{},
		Host:   host,
	}
	if err := req.WriteProxy(c); err != nil {
		slog.Debug("rpc call:", err)
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	if err != nil {
		slog.Debug("rpc call:", err)
		return err
	}
	if resp.StatusCode != http.StatusOK {
		slog.Debug("rpc call:", resp.Status)
		return fmt.Errorf("rpc call: %v", resp.Status)
	}
	client := rpc.NewClient(c)
	return client.Call(method, args, reply)
}

func (s *Server) gossip(ctx context.Context, method string, args interface{}, reply interface{}) error {
	p := func() *peer {
		s.mu.RLock()
		defer s.mu.RUnlock()
		set := make([]*peer, 0)
		for _, p := range s.peers {
			if p.isConnected() {
				set = append(set, p)
			}
		}
		if len(set) < 1 {
			return nil
		}
		return set[rand.Intn(len(set))]
	}()
	if p == nil {
		return errors.New("no connected peer")
	}
	return p.call(ctx, method, args, reply)
}

type rpcResult struct {
	reply interface{}
	err   error
}

func (s *Server) broadcast(method string, args interface{}, replyType reflect.Type) <-chan rpcResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	wg := sync.WaitGroup{}
	ch := make(chan rpcResult, 10)
	for name, p := range s.peers {
		if !p.isConnected() {
			continue
		}
		slog.Verbosef("broadcast %s: %s %v", name, method, args)
		wg.Add(1)
		go func(name string, p *peer) {
			defer wg.Done()
			ctx := s.canceller.WithTimeout(s.cfg.Timeout())
			defer s.canceller.Cancel(ctx)
			reply := reflect.New(replyType).Interface()
			err := p.call(ctx, method, args, reply)
			if err != nil {
				slog.Errorf("broadcast %s: %v", name, err)
				ch <- rpcResult{nil, err}
				return
			}
			ch <- rpcResult{reply, nil}
		}(name, p)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	return ch
}

type RPC struct {
	server *Server
	router *Router
	peer   *peer
}

func (r *RPC) Bootstrap(args *proto.PeerInfo, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Bootstrap: %v", args)
	r.peer.info = *args
	_ = r.server.broadcast("RPC.Update", args, reflect.TypeOf(proto.None{}))
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
	if r.server.Update(args) {
		_ = r.server.broadcast("RPC.Update", args, reflect.TypeOf(proto.None{}))
	}
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Sync(args *proto.Cluster, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Sync: %v", args)
	r.server.MergeCluster(args)
	*reply = *r.server.ClusterInfo()
	r.server.print()
	r.router.print()
	return nil
}

func (r *RPC) Ping(args *proto.Ping, reply *proto.Ping) error {
	slog.Verbosef("RPC.Ping: %v", args)
	name := r.server.LocalPeerName()
	*reply = proto.Ping{
		Source:      name,
		Destination: args.Destination,
	}
	if name == args.Destination {
		return nil
	}
	if args.TTL <= 0 {
		return fmt.Errorf("reply from %s: destination is unreachable (TTL exceeded): %q", name, args.Destination)
	}
	args.TTL--
	peer := r.server.getPeer(args.Destination)
	if peer == nil {
		return fmt.Errorf("reply from %s: destination is unreachable (name not resolved): %q", name, args.Destination)
	}
	ctx := r.server.canceller.WithTimeout(r.peer.cfg.Timeout())
	defer r.server.canceller.Cancel(ctx)
	if err := peer.call(ctx, "RPC.Ping", args, &proto.Ping{}); err != nil {
		return err
	}
	return nil
}
