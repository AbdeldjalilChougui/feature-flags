// Command flagd runs the feature-flag server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AbdeldjalilChougui/feature-flags/server"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	data := flag.String("data", "flags.json", "state file")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token := os.Getenv("FLAGS_ADMIN_TOKEN")
	if token == "" {
		log.Warn("FLAGS_ADMIN_TOKEN is not set: the admin API and UI are open to anyone")
	}
	st, err := server.Open(*data)
	if err != nil {
		log.Error("open", "err", err)
		os.Exit(1)
	}
	for _, e := range st.Environments() {
		fmt.Printf("environment %-12s SDK key %s\n", e.Key, e.SDKKey)
	}
	srv := &http.Server{Addr: *addr, Handler: server.New(st, server.Config{AdminToken: token, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// SSE connections never finish on their own; give them a moment,
		// then close.
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			srv.Close()
		}
	}()
	log.Info("listening", "addr", *addr, "ui", "/admin")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
