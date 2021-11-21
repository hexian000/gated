package config

import (
	"encoding/json"
	"io/ioutil"
)

type Auth struct {
	Certificate    string   `json:"cert"`
	PrivateKey     string   `json:"key"`
	AutorizedCerts []string `json:"authcerts"`
}

type Transport struct {
	NoDelay           bool   `json:"nodelay"`
	Linger            int    `json:"linger"`
	KeepAliveInterval int    `json:"keepalive"`
	Timeout           int    `json:"timeout"`
	WriteTimeout      int    `json:"writetimeout"`
	StreamWindow      uint32 `json:"window"`
}

type Server struct {
	ServerName string `json:"sni"`
	Address    string `json:"addr"`
}

type Main struct {
	Name          string            `json:"name"`
	Domain        string            `json:"vdomain"`
	ServerName    string            `json:"sni"`
	Listen        string            `json:"listen"`
	ProxyListen   string            `json:"proxylisten"`
	AdvertiseAddr string            `json:"addr"`
	RProxy        string            `json:"rproxy"`
	Servers       []Server          `json:"servers"`
	Hosts         map[string]string `json:"hosts"`
	DefaultRoute  string            `json:"default"`

	Auth      `json:"auth"`
	Transport `json:"transport"`

	UDPLog string `json:"udplog"`
}

func New() *Main {
	return &Main{
		Domain:     "lan",
		ServerName: "example.com",
		Transport: Transport{
			NoDelay:           false,
			Linger:            15,
			KeepAliveInterval: 15,
			Timeout:           15,
			WriteTimeout:      15,
			StreamWindow:      256 * 1024,
		},
	}
}

func ReadFile(filename string) (*Main, error) {
	cfg := New()
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, cfg)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
