package gated

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/hexian000/gated/proxy"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
	"github.com/hexian000/gated/version"
)

const (
	PathStatus = "/status"
	PathRPC    = "/v1/rpc"
)

type apiHandler struct {
	mux http.Handler
	s   *Server
}

func newAPIHandler(s *Server, rpcHandler http.Handler) *apiHandler {
	mux := http.NewServeMux()
	mux.Handle(PathStatus, &statusHandler{s})
	if rpcHandler != nil {
		mux.Handle(PathRPC, rpcHandler)
	}
	return &apiHandler{
		mux: mux,
		s:   s,
	}
}

func webError(s *Server, w http.ResponseWriter, msg string, code int) {
	http.Error(w, version.WebBanner(s.LocalPeerName())+msg, code)
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	hostname := req.URL.Hostname()
	apiDomain := h.s.cfg.GetFQDN(apiHost)
	if hostname == "" || strings.EqualFold(hostname, apiDomain) {
		slog.Verbose("local api:", req.URL)
		h.mux.ServeHTTP(w, req)
		return
	} else if peer, ok := util.StripDomain(hostname, apiDomain); ok && peer == h.s.LocalPeerName() {
		slog.Verbose("local peer api:", req.URL)
		h.mux.ServeHTTP(w, req)
		return
	} else if ok {
		p := h.s.getPeer(peer)
		if p == nil {
			http.Error(w, fmt.Sprintf("unknown peer: %s", peer), http.StatusNotFound)
			return
		}
		slog.Verbose("routed api:", req.URL)
		h.proxy(h.s.httpClient, w, req)
		return
	}
	if req.Method == http.MethodConnect {
		h.ServeConnect(w, req)
		return
	}
	h.proxy(h.s.httpClient, w, req)
}

// plain HTTP proxy

var hopHeaders = [...]string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

func delHopHeaders(header http.Header) {
	for _, h := range hopHeaders {
		header.Del(h)
	}
}

func (h *apiHandler) proxy(client *http.Client, w http.ResponseWriter, req *http.Request) {
	req.RequestURI = ""
	delHopHeaders(req.Header)
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			host = strings.Join(prior, ", ") + ", " + host
		}
		req.Header.Set("X-Forwarded-For", host)
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("http:", err)
		h.gatewayError(w, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	delHopHeaders(resp.Header)
	for k, v := range resp.Header {
		for _, i := range v {
			w.Header().Add(k, i)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// HTTP CONNECT

func (h *apiHandler) gatewayError(w http.ResponseWriter, err error) {
	if err, ok := err.(net.Error); ok && err.Timeout() {
		webError(h.s, w, err.Error(), http.StatusGatewayTimeout)
		return
	}
	webError(h.s, w, err.Error(), http.StatusBadGateway)
}

func (h *apiHandler) ServeConnect(w http.ResponseWriter, req *http.Request) {
	host, _, err := net.SplitHostPort(req.Host)
	if err != nil {
		webError(h.s, w, fmt.Sprintf("proxy address: %v", err), http.StatusBadRequest)
		return
	}
	addr := req.Host
	proxyURL, err := h.s.router.Resolve(host)
	if err != nil {
		webError(h.s, w, fmt.Sprintf("proxy resolve: %v", err), http.StatusBadRequest)
		return
	}
	if proxyURL != nil {
		addr = proxyURL.Host + ":80"
	}

	ctx := h.s.canceller.WithTimeout(h.s.cfg.Timeout())
	defer h.s.canceller.Cancel(ctx)
	const network = "tcp"
	slog.Debug("http connect: dial", addr)
	dialed, err := h.s.router.DialContext(ctx, network, addr)
	if err != nil {
		slog.Debug("routed dial:", err)
		h.gatewayError(w, err)
		return
	}
	if proxyURL != nil {
		slog.Debug("http connect: CONNECT ", req.Host)
		conn := proxy.Client(dialed, req.Host)
		err = conn.HandshakeContext(ctx)
		if err != nil {
			slog.Warning("http connect:", err)
			h.gatewayError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	accepted, err := proxy.Hijack(w)
	if err != nil {
		slog.Error("hijack:", err)
		return
	}
	h.s.forwarder.Forward(accepted, dialed)
}
