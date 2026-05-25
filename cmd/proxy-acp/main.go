package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"proxy-acp/internal/acpbridge"
	"proxy-acp/internal/config"
	"proxy-acp/internal/server"
)

func main() {
	envPath := flag.String("env", "", "Path to proxy-acp dotenv config")
	flag.Parse()

	cfg, err := config.Load(*envPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	manager := acpbridge.NewManager(cfg)
	defer manager.Close()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.New(cfg, manager),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[proxy-acp] listening on %s", cfg.ListenAddr)
		errCh <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-stop:
		log.Printf("[proxy-acp] shutdown signal: %s", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server failed: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[proxy-acp] graceful shutdown failed: %v", err)
	}
}
