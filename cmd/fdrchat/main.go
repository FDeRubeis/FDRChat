package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fdrchat/internal/db"
	"fdrchat/internal/message"
	"fdrchat/internal/server"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
	"fdrchat/internal/user"
)

func main() {

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {

	// Cancel context on shutdown signals
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Read environment variables
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// PostgreSQL Connection Pool
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}

	// Initialize the database schema
	if err := db.ApplySchema(ctx, pool); err != nil {
		return err
	}

	// SSE and session management
	hub := stream.NewHub()
	sessionManager := session.NewManager(pool, hub)
	go sessionManager.RunCleanup(ctx, time.Minute)

	// Create HTTP Server
	srv := &http.Server{
		Addr: addr,
		Handler: server.NewRouter(server.Deps{
			Users:    user.NewStore(pool),
			Messages: message.NewStore(pool),
			Sessions: sessionManager,
			Hub:      hub,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Run HTTP Server
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful Streams and Server Shutdown
	hub.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)

}
