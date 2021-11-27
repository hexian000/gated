package proto

type PeerInfo struct {
	Timestamp  int64    `json:"timestamp"`
	PeerName   string   `json:"name"`
	ServerName string   `json:"sni,omitempty"`
	Address    string   `json:"addr,omitempty"`
	RemoteAddr string   `json:"remote"`
	Hosts      []string `json:"hosts,omitempty"`
}

type Cluster struct {
	Self   PeerInfo            `json:"self"`
	Peers  map[string]PeerInfo `json:"peers,omitempty"`
	Routes map[string]string   `json:"routes,omitempty"`
}

type Ping struct {
	Source      string `json:"source"`
	Destination string `json:"dest"`
	TTL         int    `json:"ttl,omitempty"`
}

type None struct{}
