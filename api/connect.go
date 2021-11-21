package api

import (
	"fmt"
	"net"
	"net/http"

	"github.com/hexian000/gated/api/proxy"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/util"
)

func (s *Server) gatewayError(w http.ResponseWriter, err error) {
	if err, ok := err.(net.Error); ok && err.Timeout() {
		s.webError(w, err.Error(), http.StatusGatewayTimeout)
	} else {
		s.webError(w, err.Error(), http.StatusBadGateway)
	}
}

func (s *Server) ServeConnect(w http.ResponseWriter, req *http.Request) {
	host, _, err := net.SplitHostPort(req.Host)
	if err != nil {
		s.webError(w, fmt.Sprintf("proxy address: %v", err), http.StatusBadRequest)
		return
	}
	addr := req.Host
	proxyURL, err := s.router.Resolve(host)
	if err != nil {
		s.webError(w, fmt.Sprintf("proxy resolve: %v", err), http.StatusBadRequest)
		return
	}
	if proxyURL != nil {
		addr = proxyURL.Host + ":80"
	}

	ctx := util.WithTimeout(s.cfg.Timeout)
	defer util.Cancel(ctx)
	const network = "tcp"
	slog.Debug("http connect: dial", addr)
	dialed, err := s.router.DialContext(ctx, network, addr)
	if err != nil {
		slog.Debug("routed dial:", err)
		s.gatewayError(w, err)
		return
	}
	if proxyURL != nil {
		slog.Debug("http connect: CONNECT ", req.Host)
		conn := proxy.Client(dialed, req.Host)
		err = conn.HandshakeContext(ctx)
		if err != nil {
			s.gatewayError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	accepted, err := proxy.Hijack(w)
	if err != nil {
		slog.Error("hijack:", err)
		return
	}
	s.forwarder.Forward(accepted, dialed)
}
