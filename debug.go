package gated

import (
	"bufio"
	"fmt"
	"net/http"
	"time"

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
