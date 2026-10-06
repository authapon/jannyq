// Package server hosts the bot's HTTP endpoints. For now that is only the
// health check; webhook-based channels will register their routes here.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server is a small HTTP server with a mux channels can add routes to.
type Server struct {
	addr string
	mux  *http.ServeMux
	log  *slog.Logger
}

// New creates a server listening on addr (e.g. ":8080").
func New(addr, version string, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", version)
	})
	return &Server{addr: addr, mux: mux, log: log}
}

// Mux returns the router for registering routes before Run.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.log.Info("http server listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
}
