package config

import (
	"encoding/json"
	"io/ioutil"

	"github.com/hexian000/gated/slog"
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
	IdleTimeout       int    `json:"idletimeout"`
	StreamWindow      uint32 `json:"window"`
}

type Socks5 struct {
	Listen  string `json:"listen"`
	Forward string `json:"forward"`
}

type Server struct {
	ServerName string `json:"sni"`
	Address    string `json:"addr"`
}

type Routes struct {
	CacheTimeout int      `json:"cachetimeout"`
	Rules        []string `json:"rules"`
	Default      string   `json:"default"`
}

type Main struct {
	Name          string            `json:"name"`
	Domain        string            `json:"vdomain"`
	ServerName    string            `json:"sni"`
	Listen        string            `json:"listen"`
	HTTPListen    string            `json:"httplisten"`
	AdvertiseAddr string            `json:"addr"`
	Servers       []Server          `json:"servers"`
	Hosts         map[string]string `json:"hosts"`

	Routes    Routes    `json:"routes"`
	Auth      Auth      `json:"auth"`
	Transport Transport `json:"transport"`

	Socks5 []Socks5 `json:"socks5"`

	LogLevel int    `json:"loglevel"`
	Log      string `json:"log"`
}

func New() *Main {
	return &Main{
		Domain:     "lan",
		ServerName: "example.com",
		Routes: Routes{
			CacheTimeout: 15 * 60,
		},
		Transport: Transport{
			NoDelay:           true,
			Linger:            15,
			KeepAliveInterval: 5,
			Timeout:           15,
			WriteTimeout:      15,
			IdleTimeout:       15 * 60,
			StreamWindow:      256 * 1024,
		},
		LogLevel: slog.LevelInfo,
		Log:      "stderr",
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
