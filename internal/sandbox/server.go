package sandbox

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Backend is what the HTTP server needs from an Executor.
type Backend interface {
	Runner
	Info() Info
}

// Server exposes a Backend over HTTP. Every endpoint except /healthz
// requires the shared bearer token.
type Server struct {
	backend Backend
	token   string
	log     *slog.Logger
	timeout time.Duration // longest possible command, to size write timeouts
}

// NewServer creates the HTTP handler. token must be non-empty.
func NewServer(b Backend, token string, maxTimeout time.Duration, log *slog.Logger) (*Server, error) {
	if len(token) < 16 {
		return nil, errors.New("sandbox: token must be at least 16 characters (generate one with `openssl rand -hex 32`)")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{backend: b, token: token, log: log, timeout: maxTimeout}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /run", s.auth(http.HandlerFunc(s.handleRun)))
	mux.Handle("GET /info", s.auth(http.HandlerFunc(s.handleInfo)))
	mux.Handle("DELETE /workspace/{id}", s.auth(http.HandlerFunc(s.handleReset)))
	return mux
}

// Serve listens on addr until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      s.timeout + 30*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.log.Info("sandbox executor listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorBody{"unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxCommandBytes+4096)
	var req Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"invalid request body"})
		return
	}
	res, err := s.backend.Run(r.Context(), req)
	if err != nil {
		code, msg := statusFor(err)
		if code == http.StatusInternalServerError {
			// The client only sees a generic message; the operator needs the cause.
			s.log.Error("command failed to start", "workspace", req.Workspace, "err", err)
		}
		writeJSON(w, code, errorBody{msg})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Info())
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if err := s.backend.Reset(r.Context(), r.PathValue("id")); err != nil {
		code, msg := statusFor(err)
		writeJSON(w, code, errorBody{msg})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// statusFor maps runner errors to HTTP status codes and client-safe messages.
func statusFor(err error) (int, string) {
	switch {
	case errors.Is(err, ErrBusy):
		return http.StatusTooManyRequests, ErrBusy.Error()
	case errors.Is(err, ErrInvalidWorkspace), errors.Is(err, ErrEmptyCommand), errors.Is(err, ErrCommandTooLong):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout, "request cancelled"
	}
	return http.StatusInternalServerError, "internal sandbox error"
}
