package gated

import (
	"bufio"
	"fmt"
	"time"

	"github.com/hexian000/gated/metric"
)

func (s *Server) CollectMetrics(w *bufio.Writer) {
	now := time.Now()
	writef := func(format string, a ...interface{}) {
		_, _ = w.WriteString(fmt.Sprintf(format, a...))
	}
	func() {
		count, statusChanged := s.checkStatus()
		status := "Outage"
		if count > 0 {
			status = "Operating Normally"
		}
		writef("Status: %s, %v\n\n", status, time.Since(statusChanged))
	}()
	(&metric.Runtime{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Peers ===\n")
	cacheTimeout := s.cfg.CacheTimeout()
	for name, p := range s.getPeers() {
		info, connected := p.PeerInfo()
		if info.Online {
			writef("\nPeer %q\n", name)
		} else {
			writef("\nPeer %q (offline)\n", name)
		}
		if info.Address != "" {
			writef("    %-16s  %q\n", "Address:", info.Address)
		} else {
			writef("    %-16s  %s\n", "Address:", "(unreachable)")
		}
		lastUsed := p.LastUsed()
		status := "disconnected"
		if connected {
			numStreams := 0
			if mux := p.MuxSession(); mux != nil && !mux.IsClosed() {
				numStreams = mux.NumStreams()
			}
			if numStreams > 0 {
				status = fmt.Sprintf("%d streams", numStreams)
			} else if info.Address == "" {
				status = "linger"
			} else {
				status = "idle, " + now.Sub(lastUsed).String()
			}
		}
		writef("    %-16s  %s\n", "Status:", status)
		writef("    %-16s  %s\n", "LastUsed:", formatAgo(now, lastUsed))
		if proxy := s.router.getProxy(name, cacheTimeout); proxy == "" {
			writef("    %-16s  %s\n", "Proxy:", "(direct)")
		} else {
			writef("    %-16s  %q\n", "Proxy:", proxy)
		}
		writef("    %-16s  %s\n", "Created:", formatAgo(now, p.Created()))
		writef("    %-16s  %s\n", "Connected:", formatAgo(now, p.Connected()))
		writef("    %-16s  %s\n", "LastUpdated:", formatAgo(now, p.LastUpdate()))
		if meter := p.GetMeter(); meter != nil {
			read, written := meter.Count()
			writef("    %-16s  %s / %s\n", "Bandwidth(U/D):", formatIEC(written), formatIEC(read))
		}
		writef("    %-16s  %q\n", "Version:", info.Version)
	}
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Server ===\n\n")
	s.forwarder.CollectMetrics(w)
	s.canceller.CollectMetrics(w)
	_, _ = w.WriteString("\n")

	_, _ = w.WriteString("=== Stack ===\n\n")
	(&metric.Stack{}).CollectMetrics(w)
	_, _ = w.WriteString("\n")
}