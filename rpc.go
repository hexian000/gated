package gated

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/rpc"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/slog"
)

func (p *peer) call(conn net.Conn, deadline time.Time, method string, args interface{}, reply interface{}) error {
	conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})
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
	if err := req.WriteProxy(conn); err != nil {
		slog.Debug("rpc call:", err)
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		slog.Debug("rpc call:", err)
		return err
	}
	if resp.StatusCode != http.StatusOK {
		slog.Debug("rpc call:", resp.Status)
		return fmt.Errorf("rpc call: %v", resp.Status)
	}
	client := rpc.NewClient(conn)
	return client.Call(method, args, reply)
}

func (p *peer) Call(ctx context.Context, method string, args interface{}, reply interface{}) error {
	conn, err := p.DialContext(ctx)
	if err != nil {
		slog.Error("rpc call:", err)
		return err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Time{}
	}
	return p.call(conn, deadline, method, args, reply)
}

func (s *Server) RandomCall(ctx context.Context, method string, args interface{}, reply interface{}) error {
	p := func() *peer {
		set := make([]*peer, 0)
		for _, p := range s.getPeers() {
			if p.isReachable() || p.isConnected() {
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
	slog.Debugf("ramdom calling %s", p.info.PeerName)
	return p.Call(ctx, method, args, reply)
}

type rpcResult struct {
	from  *peer
	reply interface{}
	err   error
}

func (s *Server) Broadcast(ctx context.Context, method string, args interface{}, replyType reflect.Type) <-chan rpcResult {
	wg := sync.WaitGroup{}
	ch := make(chan rpcResult, 10)
	for name, p := range s.getPeers() {
		wg.Add(1)
		go func(name string, p *peer) {
			defer wg.Done()
			slog.Verbosef("broadcast %q on %q args: %v", method, name, args)
			reply := reflect.New(replyType).Interface()
			if err := p.Call(ctx, method, args, reply); err != nil {
				slog.Errorf("broadcast %q error from %q: %v", method, name, err)
				ch <- rpcResult{p, nil, err}
				return
			}
			slog.Verbosef("broadcast %q reply from %q: %v", method, name, reply)
			ch <- rpcResult{p, reply, nil}
		}(name, p)
	}
	go func() {
		wg.Wait()
		slog.Verbosef("broadcast %q is done", method)
		close(ch)
	}()
	return ch
}

type RPC struct {
	server *Server
	router *Router
	peer   *peer
}

func (r *RPC) Bootstrap(args *proto.Cluster, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Bootstrap: %v", args)
	r.peer.info = args.Self
	r.server.MergeCluster(args)
	go func(s *Server) {
		info := s.ClusterInfo()
		ctx := s.canceller.WithTimeout(s.cfg.Timeout())
		defer s.canceller.Cancel(ctx)
		var cluster proto.Cluster
		if err := s.RandomCall(ctx, "RPC.Update", info, &cluster); err != nil {
			slog.Debugf("call RPC.Update: %v", err)
			return
		}
		s.MergeCluster(&cluster)
	}(r.server)
	r.server.addPeer(r.peer)
	*reply = *r.server.ClusterInfo()
	return nil
}

func (r *RPC) Update(args *proto.Cluster, reply *proto.Cluster) error {
	slog.Verbosef("RPC.Update: %v", args)
	if r.server.Update(&args.Self) {
		go func(s *Server) {
			ctx := s.canceller.WithTimeout(s.cfg.Timeout())
			defer s.canceller.Cancel(ctx)
			for call := range s.Broadcast(ctx, "RPC.Update", args, reflect.TypeOf(proto.Cluster{})) {
				if call.err != nil {
					slog.Debugf("call RPC.Update: %v", call.err)
					continue
				}
				s.MergeCluster(call.reply.(*proto.Cluster))
			}
		}(r.server)
		r.server.MergeCluster(args)
	}
	*reply = *r.server.ClusterInfo()
	return nil
}

func (r *RPC) Ping(args *proto.Ping, reply *proto.Ping) error {
	slog.Verbosef("RPC.Ping: %v", args)
	name := r.server.LocalPeerName()
	args.TTL--
	if strings.EqualFold(name, args.Destination) {
		*reply = proto.Ping{
			Source:      name,
			Destination: args.Destination,
			TTL:         args.TTL,
		}
		return nil
	}
	if args.TTL <= 0 {
		return fmt.Errorf("reply from %s: destination is unreachable (TTL exceeded): %q", name, args.Destination)
	}
	peer := r.server.getPeer(args.Destination)
	if peer == nil {
		return fmt.Errorf("reply from %s: destination is unreachable (name not resolved): %q", name, args.Destination)
	}
	ctx := r.server.canceller.WithTimeout(r.peer.cfg.Timeout())
	defer r.server.canceller.Cancel(ctx)
	if err := peer.Call(ctx, "RPC.Ping", args, reply); err != nil {
		return err
	}
	return nil
}
