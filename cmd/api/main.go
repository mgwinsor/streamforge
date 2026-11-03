package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mgwinsor/streamforge/internal/config"
	"github.com/mgwinsor/streamforge/internal/httpapi"
	"github.com/mgwinsor/streamforge/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	c, err := config.Load()
	if err != nil {
		return err
	}

	root, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startup, cancel := context.WithTimeout(ctx, 10*time.Second)

	db, err := store.Open(startup, c.DatabaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()

	api := &httpapi.API{
		DB:      db,
		Sources: filepath.Join(root, "sources"),
		Log:     log,
	}

	server := &http.Server{
		Addr:              c.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Info("API listening", "addr", c.Addr)

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err = server.Shutdown(shutdown); err != nil {
		server.Close()
		return err
	}
	return nil
}
