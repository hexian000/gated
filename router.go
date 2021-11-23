package gated

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

type Router struct {
	mu sync.RWMutex

	*http.Transport
	routes map[string]string // map[host]peer
	hosts  map[string]string // map[host]addr

	domain       string
	defaultProxy *url.URL

	dialer net.Dialer
	server *Server
}

const (
	apiHost   = "api.gated"
	routeHost = "route.gated"
)

func NewRouter(domain string, defaultPeer string, server *Server, hosts map[string]string) *Router {
	r := &Router{
		routes: make(map[string]string),
		hosts:  hosts,
		domain: fmt.Sprintf("%s.%s", routeHost, domain),
		server: server,
	}
	r.Transport = &http.Transport{
		Proxy:       r.Proxy,
		DialContext: r.DialContext,
	}
	if defaultPeer != "" {
		r.defaultProxy = r.makeURL(defaultPeer)
	}
	return r
}

func (r *Router) GetRouteDomain(peer string) string {
	return peer + "." + r.domain
}

func (r *Router) makeURL(peer string) *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   r.GetRouteDomain(peer),
	}
}

func (r *Router) merge(routes map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for host, peer := range routes {
		r.routes[host] = peer
	}
}

func (r *Router) update(hosts []string, peer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, host := range hosts {
		r.routes[host] = peer
	}
}

func (r *Router) print() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	slog.Debug("router routes:", r.routes)
	slog.Debug("router hosts:", r.hosts)
}

func (r *Router) Routes() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	routes := make(map[string]string)
	for host, peer := range r.routes {
		routes[host] = peer
	}
	return routes
}

func (r *Router) Resolve(host string) (*url.URL, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.hosts[host]; ok {
		// direct
		slog.Debug("router resolve:", host, "goes direct")
		return nil, nil
	}
	if peer, ok := r.routes[host]; ok {
		// routed
		slog.Debug("router resolve:", host, "via", peer)
		return r.makeURL(peer), nil
	}
	slog.Debugf("router resolve: %s goes default (%v)", host, r.defaultProxy)
	return r.defaultProxy, nil
}

func (r *Router) Proxy(req *http.Request) (*url.URL, error) {
	return r.Resolve(req.URL.Hostname())
}

func (r *Router) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if peer, ok := util.StripDomain(host, r.domain); ok {
		return r.server.DialPeerContext(ctx, peer)
	}
	host = func(host string) string {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if host, ok := r.hosts[host]; ok {
			return host
		}
		return host
	}(host)
	return r.dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
}
