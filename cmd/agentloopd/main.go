// Command agentloopd is the multi-tenant AgentLoop daemon.
// Isolation is the existing process jail (no Docker). Auth is pluggable
// (API key, JWT HS256, admin key).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/YaoLang/agentloop/internal/daemon"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "./data", "data directory (tenants, keys, runs)")
	modelName := flag.String("model", "mock", "default model: mock | openai")
	flag.Parse()

	admin := os.Getenv("AGENTLOOP_ADMIN_KEY")
	if admin == "" {
		log.Printf("agentloopd: AGENTLOOP_ADMIN_KEY is unset; admin API disabled")
	}
	jwtSecret := os.Getenv("AGENTLOOP_JWT_SECRET")

	s, err := daemon.New(daemon.Config{
		DataDir:      *data,
		DefaultModel: *modelName,
		AdminKey:     admin,
		JWTSecret:    jwtSecret,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloopd: %v\n", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("agentloopd: listening on %s  data=%s  model=%s", *addr, *data, *modelName)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "agentloopd: %v\n", err)
			os.Exit(1)
		}
	}
}
