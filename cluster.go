package gated

import (
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
	return &proto.PeerInfo{
		Timestamp:  NewTimestamp(),
		PeerName:   cfg.Name,
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
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if p, ok := s.peers[name]; ok {
			_ = p.Close()
		}
		s.peers[name] = peer
	}()
	s.router.update(peer.info.Hosts, name)
}

func (s *Server) Peers() map[string]proto.PeerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make(map[string]proto.PeerInfo)
	for name, peer := range s.peers {
		peers[name] = peer.info
	}
	return peers
}

func (s *Server) ClusterInfo() *proto.Cluster {
	return &proto.Cluster{
		Self:   *s.self(),
		Peers:  s.Peers(),
		Routes: s.router.Routes(),
	}
}

func (s *Server) updatePeerInfo(info *proto.PeerInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.peers[info.PeerName]; ok {
		if p.info.Timestamp < info.Timestamp {
			slog.Debug("peer info update:", info)
			p.info = *info
			return true
		}
		return false
	}
	slog.Debug("peer info add:", info)
	p := newPeer(s)
	p.info = *info
	s.peers[info.PeerName] = p
	return true
}

func (s *Server) Update(info *proto.PeerInfo) bool {
	if info.PeerName == s.LocalPeerName() {
		return false
	}
	if s.updatePeerInfo(info) {
		s.router.update(info.Hosts, info.PeerName)
		return true
	}
	return false
}

func (s *Server) MergeCluster(cluster *proto.Cluster) {
	if s.Update(&cluster.Self) {
		s.router.update(cluster.Self.Hosts, cluster.Self.PeerName)
	}
	for _, info := range cluster.Peers {
		s.Update(&info)
	}
	s.router.merge(cluster.Routes)
}

func (s *Server) print() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slog.Debug("peers:", s.peers)
}
