package gated

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/hexian000/gated/proto"
	"github.com/hexian000/gated/version"
)

func formatSince(now, last time.Time) string {
	if last == (time.Time{}) {
		return "(never)"
	}
	return fmt.Sprintf("%s (since %v)", now.Sub(last), last)
}

func formatIEC(bytes uint64) string {
	units := [...]string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB", "ZiB", "YiB"}
	n := -1
	if bytes > 0 {
		n = int(math.Floor((math.Floor(math.Log2(float64(bytes)))-3.0)/10.0)) - 1
	}
	if n < 0 {
		return fmt.Sprintf("%d B", bytes)
	}
	if n >= len(units) {
		n = len(units) - 1
	}
	return fmt.Sprintf("%.01f %s", float64(bytes)/float64(uint64(2)<<((n+1)*10)), units[n])
}

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
			info, connected := p.PeerInfo()
			w := &bytes.Buffer{}
			writef := func(format string, a ...interface{}) {
				_, _ = w.WriteString(fmt.Sprintf(format, a...))
			}
			writef("%q: Address=%q, Connected=%v, LastConnected=%v\n", info.PeerName, info.Address, connected, formatSince(start, p.LastUsed()))
			ctx := h.s.canceller.WithTimeout(h.s.cfg.Timeout())
			defer h.s.canceller.Cancel(ctx)
			start := time.Now()
			for result := range h.s.Broadcast(ctx, "RPC.Ping", &proto.Ping{
				Source:      h.s.LocalPeerName(),
				Destination: info.PeerName,
				TTL:         2,
			}, reflect.TypeOf(proto.Ping{})) {
				from := result.from.info.PeerName
				if result.err != nil {
					writef("    %v: error from %q: %s\n", time.Since(start), from, result.err.Error())
					continue
				}
				reply := result.reply.(*proto.Ping)
				writef("    %v: reply from %q, TTL=%d\n", time.Since(start), from, reply.TTL)
			}
			_, _ = w.WriteString("\n")
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
		w.WriteString(fmt.Sprintf("%q at %q\n", host, peer))
	}
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("\n==========\n")
	_, _ = w.WriteString(fmt.Sprintln("Generated in", time.Since(start)))
}
