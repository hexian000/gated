package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hexian000/gated"
	"github.com/hexian000/gated/config"
	"github.com/hexian000/gated/daemon"
	"github.com/hexian000/gated/slog"
	"github.com/hexian000/gated/version"
)

var _ = version.Version

func parseFlags() string {
	var flagHelp bool
	var flagConfig string
	flag.BoolVar(&flagHelp, "h", false, "help")
	flag.StringVar(&flagConfig, "c", "", "config file")
	flag.Parse()
	if flagHelp || flagConfig == "" {
		flag.Usage()
		os.Exit(1)
	}
	return flagConfig
}

func readConfig(path string) (*config.Main, error) {
	slog.Verbose("read config:", path)
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := config.New()
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	path := parseFlags()
	cfg, err := readConfig(path)
	if err != nil {
		slog.Fatal("read config:", err)
		os.Exit(1)
	}
	c, err := gated.NewConfig(cfg)
	if err != nil {
		slog.Fatal("load config:", err)
		os.Exit(1)
	}
	slog.Default().SetLevel(cfg.LogLevel)
	if err := slog.Default().ParseOutput(cfg.Log, "gated"); err != nil {
		slog.Fatal("logging:", err)
		os.Exit(1)
	}
	server := gated.NewServer(c)
	slog.Info("server starting")
	if err := server.Start(); err != nil {
		slog.Fatal("server start:", err)
		os.Exit(1)
	}
	_, _ = daemon.Notify(daemon.Ready)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-ch
		slog.Verbose("got signal:", sig)
		if sig != syscall.SIGHUP {
			_, _ = daemon.Notify(daemon.Stopping)
			break
		}
		// reload
		_, _ = daemon.Notify(daemon.Reloading)
		cfg, err := readConfig(path)
		if err != nil {
			slog.Error("read config:", err)
			continue
		}
		slog.Default().SetLevel(cfg.LogLevel)
		if err := slog.Default().ParseOutput(cfg.Log, "gated"); err != nil {
			slog.Error("logging:", err)
			continue
		}
		if err := c.Load(cfg); err != nil {
			slog.Error("load config:", err)
			continue
		}
		_, _ = daemon.Notify(daemon.Ready)
		slog.Info("config successfully reloaded")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		slog.Fatal("server shutdown:", err)
		os.Exit(1)
	}
	slog.Info("server stopped gracefully")
}
