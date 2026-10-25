package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	configPath := flag.String("config", envOr("MINIS_CONFIG", "/data/config.json"), "path to config.json")
	listen := flag.String("listen", envOr("MINIS_LISTEN", defaultListen), "listen address")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open-minis-server: %v\n\nExample config:\n%s\n", err, ExampleConfig)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir %s: %v", cfg.DataDir, err)
	}
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	server := NewServer(cfg, store)
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	models := 0
	for _, p := range cfg.Providers {
		models += len(p.Models)
	}
	log.Printf("open-minis-server %s listening on %s (data=%s, providers=%d, models=%d, sessions=%d)",
		serverVersion, *listen, cfg.DataDir, len(cfg.Providers), models, len(store.ListSessions()))

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-sigCtx.Done()
		log.Printf("shutting down")
		server.cancelAllRuns()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("stopped")
}
