package gated

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

type Router struct {
	mu sync.RWMutex

	*http.Transport
	routes map[string]string // map[host]peer
	hosts  map[string]string // map[host]addr

	proxyMu sync.RWMutex
	proxy   map[string]peerProxy

	apiDomain   string
	routeDomain string
	defaultPeer string

	dialer net.Dialer
	server *Server
}

type peerProxy struct {
	ready   bool
	proxy   string
	updated time.Time
}

type Resolver interface {
	Resolve(host string) (*url.URL, error)
	Proxy(req *http.Request) (*url.URL, error)
}

var _ = Resolver(&Router{})

const (
	apiHost   = "api.gated"
	routeHost = "route.gated"
)

func NewRouter(server *Server) *Router {
	cfg := server.cfg.Current()
	r := &Router{
		routes:      make(map[string]string),
		hosts:       cfg.Hosts,
		apiDomain:   server.cfg.GetFQDN(apiHost),
		routeDomain: server.cfg.GetFQDN(routeHost),
		server:      server,
		proxy:       make(map[string]peerProxy),
		defaultPeer: cfg.Routes.Default,
	}
	r.Transport = &http.Transport{
		Proxy:             r.Proxy,
		DialContext:       r.DialContext,
		DisableKeepAlives: true,
	}
	return r
}

func (r *Router) makeURL(peer string) *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   peer + "." + r.routeDomain,
	}
}

func (r *Router) merge(routes map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for host, peer := range routes {
		r.routes[host] = peer
		slog.Verbosef("merge route: %q via %q", host, peer)
	}
}

func (r *Router) update(hosts []string, peer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, host := range hosts {
		r.routes[host] = peer
		slog.Verbosef("update route: %q via %q", host, peer)
	}
}

func (r *Router) deletePeer(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for host, peer := range r.routes {
		if peer == name {
			delete(r.routes, host)
		}
	}
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

func (r *Router) resolve(host string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.hosts[host]; ok {
		// direct
		return ""
	}
	peer, ok := r.routes[host]
	if !ok {
		// default
		peer = r.defaultPeer
	}
	return peer
}

func (r *Router) Resolve(host string) (*url.URL, error) {
	peer := r.resolve(host)
	if peer == "" {
		slog.Verbosef("router resolve: %q: goes direct", host)
		return nil, nil
	}
	if proxy := r.getProxy(peer, r.server.cfg.CacheTimeout()); proxy != "" {
		slog.Debugf("router resolve: %q: %q via %q", host, peer, proxy)
		return r.makeURL(proxy), nil
	}
	slog.Debugf("router resolve: %q: %s", host, peer)
	return r.makeURL(peer), nil
}

func (r *Router) Proxy(req *http.Request) (*url.URL, error) {
	return r.Resolve(req.URL.Hostname())
}

func (r *Router) getProxy(destination string, timeout time.Duration) string {
	r.proxyMu.RLock()
	defer r.proxyMu.RUnlock()
	info, ok := r.proxy[destination]
	if !ok {
		return ""
	}
	if time.Since(info.updated) > timeout {
		go r.findProxy(destination, true)
		return info.proxy
	}
	return info.proxy
}

func (r *Router) setProxy(destination, proxy string) {
	r.proxyMu.Lock()
	defer r.proxyMu.Unlock()

	oldProxy := ""
	if info, ok := r.proxy[destination]; ok {
		oldProxy = info.proxy
	}
	if oldProxy != proxy {
		defer slog.Infof("proxy to %q changed: %q -> %q", destination, oldProxy, proxy)
	}

	if proxy == "" || proxy == destination {
		delete(r.proxy, destination)
		return
	}
	r.proxy[destination] = peerProxy{
		ready:   true,
		proxy:   proxy,
		updated: time.Now(),
	}
}

func (r *Router) findProxy(destination string, tryDirect bool) {
	if !func(destination string) bool {
		r.proxyMu.Lock()
		defer r.proxyMu.Unlock()
		info, ok := r.proxy[destination]
		if !ok {
			r.proxy[destination] = peerProxy{
				ready: false,
			}
			return true
		}
		return info.ready
	}(destination) {
		return
	}
	slog.Verbosef("updating proxy to %q", destination)
	proxy, err := r.server.FindProxy(destination, tryDirect, true)
	if err != nil {
		proxy, err = r.server.FindProxy(destination, tryDirect, false)
		if err != nil {
			slog.Error("find proxy:", err)
		}
	}
	r.setProxy(destination, proxy)
}

func (r *Router) dialProxyContext(ctx context.Context, peer string) (net.Conn, error) {
	conn, err := r.server.DialPeerContext(ctx, peer)
	if err == nil {
		return conn, nil
	}
	go r.findProxy(peer, false)
	return nil, err
}

func (r *Router) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if peer, ok := util.StripDomain(host, r.routeDomain); ok {
		return r.dialProxyContext(ctx, peer)
	}
	if peer, ok := util.StripDomain(host, r.apiDomain); ok {
		return r.dialProxyContext(ctx, peer)
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
