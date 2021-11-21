package gated

import (
	"time"

	"github.com/hexian000/gated/api/proto"
	"github.com/hexian000/gated/slog"
)

func (s *Server) getPeer(name string) *Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peers[name]
}

func (s *Server) addPeer(peer *Peer) {
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.peers[peer.Info.PeerName] = peer
	}()
	s.updateRoute(peer.Info)
}

func (s *Server) Peers() map[string]proto.PeerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	peers := make(map[string]proto.PeerInfo)
	for host, peer := range s.peers {
		if peer.Info.Address != "" {
			peers[host] = *peer.Info
		}
	}
	return peers
}

func (s *Server) ClusterInfo() *proto.Cluster {
	myName := func() string {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.self.PeerName
	}()
	routes := s.router.Routes()
	for host, name := range routes {
		peer := s.getPeer(name)
		if peer == nil {
			continue
		}
		if peer.rproxy {
			routes[host] = myName
			continue
		}
		if peer.Info.Address == "" {
			delete(routes, host)
			continue
		}
	}
	return &proto.Cluster{
		Self:   *s.self,
		Peers:  s.Peers(),
		Routes: routes,
	}
}

func (s *Server) merge(info *proto.PeerInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.peers[info.PeerName]; ok {
		if p.Info.Timestamp < info.Timestamp {
			slog.Debug("peers merge update:", info)
			p.Info = info
			p.LastSeen = time.Now()
		}
		return
	} else {
		slog.Debug("peers merge add:", info)
		now := time.Now()
		s.peers[info.PeerName] = &Peer{
			Info:     info,
			Created:  now,
			LastSeen: now,
		}
	}
}

func (s *Server) Merge(data *proto.Cluster) {
	s.merge(&data.Self)
	for _, info := range data.Peers {
		info := info
		s.merge(&info)
	}
	slog.Debug("peers merged:", s.peers)
	s.router.merge(data.Routes)
}
