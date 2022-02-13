package gated

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexian000/gated/proxy"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
	"github.com/hexian000/gated/version"
)

const (
	PathStatus  = "/status"
	PathCluster = "/cluster"
	PathRPC     = "/v1/rpc"
)

type apiHandler struct {
	mux http.Handler
	s   *Server
}

func newAPIHandler(s *Server, rpcHandler http.Handler) *apiHandler {
	mux := http.NewServeMux()
	mux.Handle(PathStatus, &statusHandler{s})
	mux.Handle(PathCluster, &clusterHandler{s})
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
			webError(h.s, w, fmt.Sprintf("unknown peer: %q", peer), http.StatusNotFound)
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
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func delHopHeaders(header http.Header) {
	for _, h := range hopHeaders {
		header.Del(h)
	}
}

var ErrUnsupportedTransferEncoding = errors.New("unsupported transfer encoding")

// serveResponse serves HTTP response to http.ResponseWriter.
func serveResponse(w http.ResponseWriter, resp *http.Response) error {
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	h := w.Header()
	for k, v := range resp.Header {
		for _, v1 := range v {
			h.Add(k, v1)
		}
	}
	delHopHeaders(h)
	if h.Get("Date") == "" {
		h.Set("Date", time.Now().UTC().Format("Mon, 2 Jan 2006 15:04:05")+" GMT")
	}
	if h.Get("Content-Type") == "" && resp.ContentLength != 0 {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if resp.ContentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	} else {
		h.Del("Content-Length")
	}
	te := ""
	if len(resp.TransferEncoding) > 0 {
		if len(resp.TransferEncoding) > 1 {
			return ErrUnsupportedTransferEncoding
		}
		te = resp.TransferEncoding[0]
	}
	clientConnection := ""
	if resp.Request != nil {
		clientConnection = resp.Request.Header.Get("Connection")
	}
	switch clientConnection {
	case "close":
		h.Set("Connection", "close")
	case "keep-alive":
		if h.Get("Content-Length") != "" || te == "chunked" {
			h.Set("Connection", "keep-alive")
		} else {
			h.Set("Connection", "close")
		}
	default:
		if te == "chunked" {
			h.Set("Connection", "close")
		}
	}
	switch te {
	case "":
		w.WriteHeader(resp.StatusCode)
		if resp.Body != nil {
			if _, err := io.Copy(w, resp.Body); err != nil {
				return err
			}
		}
	case "chunked":
		h.Set("Transfer-Encoding", "chunked")
		h.Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		if resp.Body != nil {
			if _, err := io.Copy(w, resp.Body); err != nil {
				return err
			}
		}
	default:
		return ErrUnsupportedTransferEncoding
	}
	return nil
}

func (h *apiHandler) proxy(client *http.Client, w http.ResponseWriter, req *http.Request) {
	if req.URL.Scheme != "" && req.URL.Scheme != "http" {
		webError(h.s, w, fmt.Sprintf("unsupported URL scheme: %s", req.URL.Scheme), http.StatusBadRequest)
		return
	}
	delHopHeaders(req.Header)
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			host = strings.Join(prior, ", ") + ", " + host
		}
		req.Header.Set("X-Forwarded-For", host)
	}
	req.RequestURI = ""
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("http:", err)
		h.gatewayError(w, err)
		return
	}
	if err := serveResponse(w, resp); err != nil {
		slog.Debug("http:", err)
		return
	}
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
		dialed, err = proxy.Client(ctx, dialed, req.Host)
		if err != nil {
			slog.Warning("http connect:", err)
			h.gatewayError(w, err)
			return
		}
	}
	accepted, err := proxy.Hijack(w)
	if err != nil {
		slog.Error("hijack:", err)
		return
	}
	_, err = accepted.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	if err != nil {
		slog.Error("http connect:", err)
		return
	}
	h.s.forwarder.Forward(accepted, dialed)
}
