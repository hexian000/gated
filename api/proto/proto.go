package proto

type PeerInfo struct {
	Timestamp  int64    `json:"timestamp"`
	PeerName   string   `json:"name"`
	ServerName string   `json:"sni,omitempty"`
	Address    string   `json:"addr,omitempty"`
	RemoteAddr string   `json:"remote"`
	RProxy     string   `json:"rproxy,omitempty"`
	Hosts      []string `json:"hosts,omitempty"`
}

type Cluster struct {
	Self   PeerInfo            `json:"self"`
	Peers  map[string]PeerInfo `json:"peers,omitempty"`
	Routes map[string]string   `json:"routes,omitempty"`
}

type None struct{}

func (i *PeerInfo) Clone() *PeerInfo {
	shallowCopy := *i
	return &shallowCopy
}
