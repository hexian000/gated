package gated

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/version"
)

type statusHandler struct {
	s *Server
}

func (h *statusHandler) ServeHTTP(respWriter http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		webError(h.s, respWriter, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	respWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	respWriter.Header().Set("X-Content-Type-Options", "nosniff")
	start := time.Now()
	w := bufio.NewWriter(respWriter)
	defer func() {
		_ = w.Flush()
	}()
	_, _ = w.WriteString(version.WebBanner(h.s.LocalPeerName()))
	h.s.CollectMetrics(w)
	_, _ = w.WriteString("\n==========\n")
	_, _ = w.WriteString(fmt.Sprintln("Generated in", time.Since(start)))
}

type clusterHandler struct {
	s *Server
}

func (h *clusterHandler) ServeHTTP(respWriter http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		webError(h.s, respWriter, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	respWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	respWriter.Header().Set("X-Content-Type-Options", "nosniff")
	start := time.Now()
	w := bufio.NewWriter(respWriter)
	defer func() {
		_ = w.Flush()
	}()
	_, _ = w.WriteString(version.WebBanner(h.s.LocalPeerName()))

	_, _ = w.WriteString("=== Peers ===\n\n")
	wg := sync.WaitGroup{}
	ch := make(chan string, 10)
	for _, p := range h.s.getPeers() {
		wg.Add(1)
		go func(p *peer) {
			defer wg.Done()
			name := p.info.PeerName
			w := &bytes.Buffer{}
			w.WriteString(fmt.Sprintf("%s: %v, %v, %v\n", name, p.hasAddress(), p.isConnected(), time.Since(p.LastUsed())))
			ctx := h.s.canceller.WithTimeout(h.s.cfg.Timeout())
			defer h.s.canceller.Cancel(ctx)
			start := time.Now()
			for result := range h.s.Broadcast(ctx, "RPC.Ping", &proto.Ping{
				Source:      h.s.LocalPeerName(),
				Destination: name,
				TTL:         2,
			}, reflect.TypeOf(proto.Ping{})) {
				from := result.from.info.PeerName
				if result.err != nil {
					w.WriteString(fmt.Sprintf("    %v: error from %s: %s\n", time.Since(start), from, result.err.Error()))
					continue
				}
				reply := result.reply.(*proto.Ping)
				w.WriteString(fmt.Sprintf("    %v: reply from %s, TTL=%d\n", time.Since(start), from, reply.TTL))
			}
			ch <- w.String()
		}(p)
	}
	go func() {
		wg.Wait()
		close(ch)
	}()
	for s := range ch {
		_, _ = w.WriteString(s)
	}
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Routes ===\n\n")
	for host, peer := range h.s.router.Routes() {
		w.WriteString(fmt.Sprintf("%q via %s\n", host, peer))
	}
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("\n==========\n")
	_, _ = w.WriteString(fmt.Sprintln("Generated in", time.Since(start)))
}
