package gated

import (
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/hexian000/gated/config"
)

type Config struct {
	main *config.Main
	mu   sync.RWMutex
	tls  *tls.Config
	mux  *yamux.Config

	timestamp time.Time
}

func NewConfig(cfg *config.Main) (*Config, error) {
	c := &Config{}
	if err := c.Load(cfg); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) Load(cfg *config.Main) error {
	cert, err := tls.LoadX509KeyPair(cfg.Auth.Certificate, cfg.Auth.PrivateKey)
	if err != nil {
		return err
	}
	certPool := x509.NewCertPool()
	for _, path := range cfg.Auth.AutorizedCerts {
		certBytes, err := ioutil.ReadFile(path)
		if err != nil {
			return err
		}
		ok := certPool.AppendCertsFromPEM(certBytes)
		if !ok {
			return err
		}
	}
	sni := cfg.ServerName
	if sni == "" {
		sni = "example.com"
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    certPool,
		RootCAs:      certPool,
		ServerName:   sni,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	}

	timeout := time.Duration(cfg.Transport.Timeout) * time.Second
	enableKeepAlive := cfg.Transport.KeepAliveInterval > 0
	keepAliveInterval := time.Duration(cfg.Transport.KeepAliveInterval) * time.Second
	if !enableKeepAlive {
		keepAliveInterval = 15 * time.Second
	}
	muxCfg := &yamux.Config{
		AcceptBacklog:          8,
		EnableKeepAlive:        enableKeepAlive,
		KeepAliveInterval:      keepAliveInterval,
		ConnectionWriteTimeout: time.Duration(cfg.Transport.WriteTimeout) * time.Second,
		MaxStreamWindowSize:    cfg.Transport.StreamWindow,
		StreamOpenTimeout:      timeout,
		StreamCloseTimeout:     timeout,
		Logger:                 newYamuxLogger(),
	}
	if err = yamux.VerifyConfig(muxCfg); err != nil {
		return err
	}

	// everything OK, replace config
	c.mu.Lock()
	defer c.mu.Unlock()
	c.main = cfg
	c.tls = tlsCfg
	c.mux = muxCfg
	c.timestamp = time.Now()
	return nil
}

func (c *Config) Timestamp() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.timestamp.UnixMilli()
}

func (c *Config) SetConnParams(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		cfg := c.Current()
		_ = tcpConn.SetNoDelay(cfg.Transport.NoDelay)
		_ = tcpConn.SetLinger(cfg.Transport.Linger)
		_ = tcpConn.SetKeepAlive(false) // we have an encrypted one
	}
}

func (c *Config) TLSConfig(sni string) *tls.Config {
	// Clone() is goroutine safe
	cfg := c.tls.Clone()
	if sni == "" {
		sni = "example.com"
	}
	cfg.ServerName = sni
	return cfg
}

func (c *Config) MuxConfig() *yamux.Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mux
}

func (c *Config) Current() *config.Main {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.main
}

func (c *Config) Timeout() time.Duration {
	return time.Duration(c.Current().Transport.Timeout) * time.Second
}

func (c *Config) CacheTimeout() time.Duration {
	return time.Duration(c.Current().Routes.CacheTimeout) * time.Second
}

func (c *Config) GetFQDN(host string) string {
	return host + "." + c.Current().Domain
}
