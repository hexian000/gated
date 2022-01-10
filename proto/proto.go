package proto

type PeerInfo struct {
	Timestamp  int64    `json:"timestamp"`
	Version    string   `json:"version"`
	PeerName   string   `json:"name"`
	Online     bool     `json:"online"`
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

type Lookup struct {
	Source      string `json:"source"`
	Destination string `json:"dest"`
	TTL         int    `json:"ttl,omitempty"`
	Fast        bool   `json:"fast"`
}

type None struct{}
