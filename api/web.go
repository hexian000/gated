package api

import (
	"bufio"
	"fmt"
	"net/http"
	"time"

	"github.com/hexian000/gated/version"
)

func (s *Server) webError(w http.ResponseWriter, msg string, code int) {
	http.Error(w, version.WebBanner(s.GetHost())+msg, code)
}

func (s *Server) handleStatus(respWriter http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		s.webError(respWriter, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	respWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	respWriter.Header().Set("X-Content-Type-Options", "nosniff")
	start := time.Now()
	w := bufio.NewWriter(respWriter)
	defer func() {
		_ = w.Flush()
	}()
	_, _ = w.WriteString(version.WebBanner(s.GetHost()))
	s.metric.CollectMetrics(w)
	_, _ = w.WriteString("\n==========\n")
	_, _ = w.WriteString(fmt.Sprintln("Generated in", time.Since(start)))
}

func (s *Server) handleCluster(respWriter http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		s.webError(respWriter, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	start := time.Now()
	respWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	respWriter.Header().Set("X-Content-Type-Options", "nosniff")
	w := bufio.NewWriter(respWriter)
	defer func() {
		_ = w.Flush()
	}()
	_, _ = w.WriteString(version.WebBanner(s.GetHost()))
	// for name := range h.Server.dials {
	// 	_, _ = w.WriteString(fmt.Sprintf("remote: %s\n", name))
	// }
	_, _ = w.WriteString("\n==========\n")
	_, _ = w.WriteString(fmt.Sprintln("Generated in", time.Since(start)))
}
