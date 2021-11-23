package api

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hexian000/gated/api/proxy"
	"github.com/hexian000/gated/metric"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

const (
	apiHost = "api.gated"
)

type Cluster interface {
	LocalPeerName() string
	DialPeerContext(ctx context.Context, peer string) (net.Conn, error)
}

type Router interface {
	Resolve(host string) (*url.URL, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Config struct {
	Name      string
	Domain    string
	Transport *http.Transport
	Timeout   time.Duration
	Metric    metric.Metrical
	Router
	Cluster
}

type Server struct {
	*http.Server
	cfg       *Config
	mux       *http.ServeMux
	forwarder *proxy.Forwarder
	client    *http.Client
	metric    metric.Metrical
	router    Router
	peers     Cluster
}

func New(cfg *Config) *Server {
	s := &Server{
		cfg: cfg,
		Server: &http.Server{
			ReadHeaderTimeout: cfg.Timeout,
		},
		mux:       http.NewServeMux(),
		forwarder: proxy.NewForwarder(),
		metric:    cfg.Metric,
		router:    cfg.Router,
		peers:     cfg.Cluster,
	}
	s.Server.Handler = s
	s.mux = http.NewServeMux()
	s.mux.HandleFunc(WebCluster, s.handleCluster)
	s.mux.HandleFunc(WebStatus, s.handleStatus)
	s.client = &http.Client{
		Transport: cfg.Transport,
		Timeout:   cfg.Timeout,
	}
	return s
}

func (s *Server) GetAPIDomain() string {
	return apiHost + "." + s.cfg.Domain
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		s.ServeConnect(w, req)
		return
	}
	hostname := req.URL.Hostname()
	apiDomain := s.GetAPIDomain()

	if hostname == "" || strings.EqualFold(hostname, apiDomain) {
		slog.Verbose("== url:", req.URL)
		s.mux.ServeHTTP(w, req)
		return
	} else if peer, ok := util.StripDomain(hostname, apiDomain); ok && peer == s.peers.LocalPeerName() {
		slog.Verbose("== url:", req.URL)
		s.mux.ServeHTTP(w, req)
		return
	} else if ok {
		slog.Verbose("== url2:", req.URL)
		client := s.apiClient(peer)
		s.proxy(client, w, req)
		return
	}
	s.proxy(s.client, w, req)
}

func (s *Server) CollectMetrics(w *bufio.Writer) {
	s.forwarder.CollectMetrics(w)
	util.DefaultCanceller.CollectMetrics(w)
}
