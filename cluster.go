package gated

import (
	"reflect"
	"time"

	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/slog"
)

func NewTimestamp() int64 {
	return time.Now().UnixMilli()
}

func (s *Server) self() *proto.PeerInfo {
	cfg := s.cfg.Current()
	hosts := []string(nil)
	for host := range cfg.Hosts {
		hosts = append(hosts, host)
	}
	online := true
	select {
	case <-s.shutdownCh:
		online = false
	default:
	}
	return &proto.PeerInfo{
		Timestamp:  NewTimestamp(),
		PeerName:   cfg.Name,
		Online:     online,
		ServerName: cfg.ServerName,
		Address:    cfg.AdvertiseAddr,
		Hosts:      hosts,
	}
}

func (s *Server) getPeer(name string) *peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[name]
}

func (s *Server) addPeer(peer *peer) {
	name := peer.info.PeerName
	slog.Debugf("add peer: %s", name)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if p, ok := s.peers[name]; ok && p != peer {
			_ = p.Close()
		}
		s.peers[name] = peer
	}()
	s.router.update(peer.info.Hosts, name)
}

// func (s *Server) deletePeer(name string) {
// 	s.mu.Lock()
// 	defer s.mu.Unlock()
// 	if p, ok := s.peers[name]; ok {
// 		_ = p.Close()
// 		delete(s.peers, name)
// 	}
// }

func (s *Server) getPeers() map[string]*peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make(map[string]*peer)
	for name, peer := range s.peers {
		peers[name] = peer
	}
	return peers
}

func (s *Server) ClusterInfo() *proto.Cluster {
	return &proto.Cluster{
		Self: *s.self(),
		Peers: func() map[string]proto.PeerInfo {
			s.mu.RLock()
			defer s.mu.RUnlock()
			peers := make(map[string]proto.PeerInfo)
			for name, peer := range s.peers {
				peers[name] = peer.info
			}
			return peers
		}(),
		Routes: s.router.Routes(),
	}
}

func (s *Server) updatePeerInfo(info *proto.PeerInfo) bool {
	if info.PeerName == s.LocalPeerName() {
		return false
	}
	if p := s.getPeer(info.PeerName); p != nil {
		return p.UpdateInfo(info)
	}
	slog.Debug("peer info add:", info)
	p := newPeer(s)
	p.info = *info
	p.lastUpdate = time.Now()
	s.addPeer(p)
	return true
}

func (s *Server) MergeCluster(cluster *proto.Cluster) bool {
	changed := false
	if s.updatePeerInfo(&cluster.Self) {
		changed = true
		s.router.update(cluster.Self.Hosts, cluster.Self.PeerName)
	}
	for _, info := range cluster.Peers {
		if s.updatePeerInfo(&info) {
			changed = true
		}
	}
	s.router.merge(cluster.Routes)
	return changed
}

func (s *Server) broatcastUpdate(args *proto.Cluster) {
	ctx := s.canceller.WithTimeout(s.cfg.Timeout())
	defer s.canceller.Cancel(ctx)
	for call := range s.Broadcast(ctx, "RPC.Update", args.Self.PeerName, args, reflect.TypeOf(proto.Cluster{})) {
		if call.err != nil {
			slog.Debugf("call RPC.Update: %v", call.err)
			continue
		}
		s.MergeCluster(call.reply.(*proto.Cluster))
	}
}
