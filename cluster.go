package gated

import (
	"time"

	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
)

func (s *Server) self() proto.PeerInfo {
	cfg := s.cfg.Current()
	hosts := []string(nil)
	for host := range cfg.Hosts {
		hosts = append(hosts, host)
	}
	return proto.PeerInfo{
		PeerName:   cfg.Name,
		ServerName: cfg.ServerName,
		Address:    cfg.AdvertiseAddr,
		Hosts:      hosts,
	}
}

func (s *Server) getPeer(name string) *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[name]
}

func (s *Server) addPeer(peer *Peer) {
	name := peer.Info.PeerName
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if peer, ok := s.peers[name]; ok && peer.Session != nil {
			_ = peer.Session.Close()
		}
		s.peers[name] = peer
	}()
	s.router.update(peer.Info.Hosts, name)
}

func (s *Server) Peers() map[string]proto.PeerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make(map[string]proto.PeerInfo)
	for host, peer := range s.peers {
		if peer.Info.Address != "" {
			peers[host] = peer.Info
		}
	}
	return peers
}

func (s *Server) ClusterInfo() *proto.Cluster {
	return &proto.Cluster{
		Self:   s.self(),
		Peers:  s.Peers(),
		Routes: s.router.Routes(),
	}
}

func (s *Server) merge(info proto.PeerInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.peers[info.PeerName]; ok {
		if p.Info.Timestamp < info.Timestamp {
			slog.Debug("peers merge update:", info)
			p.Info = info
			p.LastSeen = time.Now()
			return true
		}
		return false
	}
	slog.Debug("peers merge add:", info)
	now := time.Now()
	s.peers[info.PeerName] = &Peer{
		Info:     info,
		Created:  now,
		LastSeen: now,
	}
	return true
}

func (s *Server) Update(info proto.PeerInfo) bool {
	if info.PeerName == s.self().PeerName {
		return false
	}
	if s.merge(info) {
		s.router.update(info.Hosts, info.PeerName)
		return true
	}
	return false
}

func (s *Server) Merge(data *proto.Cluster) {
	if s.merge(data.Self) {
		s.router.update(data.Self.Hosts, data.Self.PeerName)
	}
	for _, info := range data.Peers {
		s.Update(info)
	}
	s.router.merge(data.Routes)
}

func (s *Server) print() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slog.Debug("peers:", s.peers)
}
