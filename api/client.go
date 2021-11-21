package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

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

func (s *Server) proxy(client *http.Client, w http.ResponseWriter, req *http.Request) {
	ctx := util.WithTimeout(s.cfg.Timeout)
	defer util.Cancel(ctx)

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
		s.gatewayError(w, err)
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

func (s *Server) apiClient(peer string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return s.peers.DialPeerContext(ctx, peer)
			},
			DisableKeepAlives: true,
		},
		Timeout: s.cfg.Timeout,
	}
}
