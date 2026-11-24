// Command router is the M2 model router: one OpenAI-compatible endpoint for
// OpenCode, with rate limiting, cooldowns and ordered fallback.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jerryjuche/padi/internal/router"
	"github.com/jerryjuche/padi/internal/router/config"
)

func main() {
	path := os.Getenv("ROUTER_CONFIG")
	if path == "" {
		path = "config/router.yaml"
	}
	cfg, err := config.Load(path, os.Getenv)
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	for _, warning := range cfg.Warnings {
		log.Printf("router: warning: %s", warning)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           router.NewHandler(cfg, nil),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("router ready (listen=%s, aliases=%v)", cfg.Listen, cfg.EnabledAliases())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Printf("router: stopping on %s", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("router: listen: %v", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("router: shutdown: %v", err)
	}
}
