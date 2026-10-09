package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/authapon/jannyq/internal/backup"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/session"
)

// newMetrics creates the registry when metrics are switched on; otherwise the
// instruments it returns do nothing.
func newMetrics(cfg *config.Config, version string) (*metrics.Registry, metrics.Instruments) {
	if cfg.MetricsListen == "" {
		return nil, metrics.NewInstruments(nil)
	}
	reg := metrics.New()
	reg.RegisterRuntime(version)
	return reg, metrics.NewInstruments(reg)
}

// registerGauges adds the metrics that are read from live state at every scrape.
func registerGauges(reg *metrics.Registry, sessions *session.Manager, k *knowledgeBase) {
	reg.Gauge1("jannyq_open_sessions", "Chat databases currently open.", func() float64 { return float64(sessions.OpenCount()) })
	if k == nil {
		return
	}
	reg.GaugeFunc("jannyq_knowledge_files", "Files in the knowledge base by status.", []string{"status"}, func() []metrics.Sample {
		st, err := k.store.Stats(context.Background())
		if err != nil {
			return nil
		}
		return []metrics.Sample{{Labels: []string{"ok"}, Value: float64(st.Files)}, {Labels: []string{"failed"}, Value: float64(st.Failed)}}
	})
	reg.GaugeFunc("jannyq_knowledge_passages", "Passages in the knowledge base, with and without embeddings.", []string{"embedded"}, func() []metrics.Sample {
		st, err := k.store.Stats(context.Background())
		if err != nil {
			return nil
		}
		return []metrics.Sample{{Labels: []string{"true"}, Value: float64(st.Embedded)}, {Labels: []string{"false"}, Value: float64(st.Chunks - st.Embedded)}}
	})
	reg.Gauge1("jannyq_knowledge_indexing_pending_files", "Files the current knowledge scan still has to handle.", func() float64 {
		p := k.ix.Progress()
		if !p.Scanning {
			return 0
		}
		return float64(max(p.Total-p.Done, 0))
	})
	reg.Gauge1("jannyq_knowledge_last_scan_timestamp_seconds", "When the last knowledge scan finished, in Unix seconds (0 = none yet).", func() float64 {
		if t := k.ix.Progress().LastScan; !t.IsZero() {
			return float64(t.Unix())
		}
		return 0
	})
}

// serveMetrics serves /metrics on its own address until ctx ends, so that it
// is never reachable through the public web server.
func serveMetrics(ctx context.Context, cfg *config.Config, reg *metrics.Registry, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.MetricsListen)
	if err != nil {
		return err
	}
	if host, _, err := net.SplitHostPort(cfg.MetricsListen); err == nil {
		if ip := net.ParseIP(host); (host == "" || ip == nil || !ip.IsLoopback()) && cfg.MetricsToken == "" {
			log.Warn("the metrics are open to every address that can reach " + cfg.MetricsListen + " (set --metrics-token, or listen on 127.0.0.1)")
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", reg.Handler(cfg.MetricsToken))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	log.Info("metrics listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// runBackups makes a backup every cfg.BackupInterval (the first one as soon as
// the newest existing backup is that old, or a minute after start when there is
// none) and keeps the newest cfg.BackupKeep.
func runBackups(ctx context.Context, cfg *config.Config, version string, inst metrics.Instruments, reg *metrics.Registry, log *slog.Logger) {
	runBackupsEvery(ctx, cfg, version, inst, reg, log, time.Minute, time.Hour)
}

func runBackupsEvery(ctx context.Context, cfg *config.Config, version string, inst metrics.Instruments, reg *metrics.Registry, log *slog.Logger, firstDelay, retry time.Duration) {
	wait := firstDelay
	var lastOK atomic.Int64 // Unix seconds of the newest backup (0 = none)
	if last := backup.Latest(cfg.BackupDir); !last.IsZero() {
		lastOK.Store(last.Unix())
		wait = max(cfg.BackupInterval-time.Since(last), firstDelay)
	}
	reg.Gauge1("jannyq_last_backup_timestamp_seconds", "When the newest backup was made, in Unix seconds (0 = none).", func() float64 { return float64(lastOK.Load()) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		p, err := backup.Create(ctx, cfg.BackupDir, backup.Options{
			DataDir: cfg.DataDir, KnowledgeDB: cfg.KnowledgeDB, Files: cfg.BackupFiles, Version: version,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			inst.Backups.Inc("error")
			log.Error("backup failed", "err", err)
			wait = min(retry, cfg.BackupInterval)
			continue
		}
		inst.Backups.Inc("ok")
		lastOK.Store(time.Now().Unix())
		if n, err := backup.Prune(cfg.BackupDir, cfg.BackupKeep); err != nil {
			log.Warn("could not delete old backups", "err", err)
		} else {
			log.Info("backup written", "file", p, "deleted_old", n)
		}
		wait = cfg.BackupInterval
	}
}

// runRetention deletes chats that have been idle for cfg.RetentionDays.
func runRetention(ctx context.Context, cfg *config.Config, sessions *session.Manager, inst metrics.Instruments, log *slog.Logger, after ...func()) {
	runRetentionEvery(ctx, cfg, sessions, inst, log, time.Minute, time.Hour, after...)
}

// runRetentionEvery deletes idle chats; after each sweep it calls the after hooks.
func runRetentionEvery(ctx context.Context, cfg *config.Config, sessions *session.Manager, inst metrics.Instruments, log *slog.Logger, first, every time.Duration, after ...func()) {
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		n, err := sessions.Sweep(time.Duration(cfg.RetentionDays)*24*time.Hour, time.Now())
		if err != nil {
			log.Warn("retention sweep failed", "err", err)
		}
		if n > 0 {
			inst.Retention.Add(float64(n), "chats")
			log.Info("idle chats deleted", "count", n, "older_than_days", cfg.RetentionDays)
		}
		for _, f := range after {
			f()
		}
		wait = every
	}
}

// readyHandler answers /readyz: can the bot do its job? It checks that the
// data directory is writable and that the knowledge database answers; it does
// not depend on the model or on any platform, which would make a short outage
// elsewhere restart the bot for nothing. Results are cached for a few seconds.
func readyHandler(dataDir string, k *knowledgeBase) http.Handler {
	var mu sync.Mutex
	var at time.Time
	var cached map[string]string
	check := func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(at) < 5*time.Second {
			return cached
		}
		res := map[string]string{"data_dir": "ok"}
		f, err := os.CreateTemp(dataDir, ".ready-")
		if err != nil {
			res["data_dir"] = "not writable: " + err.Error()
		} else {
			f.Close()
			os.Remove(f.Name())
		}
		if k != nil {
			res["knowledge"] = "ok"
			if _, err := k.store.Stats(context.Background()); err != nil {
				res["knowledge"] = "unavailable: " + err.Error()
			}
		}
		cached, at = res, time.Now()
		return res
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := check()
		status, code := "ok", http.StatusOK
		for _, v := range res {
			if v != "ok" {
				status, code = "unavailable", http.StatusServiceUnavailable
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": res})
	})
}

// checkDataDir warns about a data directory that other users of the machine
// can read: it holds conversations, files and the key that signs web sessions.
func checkDataDir(dir string, log *slog.Logger) {
	fi, err := os.Stat(dir)
	if err != nil || runtime.GOOS == "windows" {
		return
	}
	if fi.Mode().Perm()&0o007 != 0 {
		log.Warn("the data directory can be read by every user of this machine; it holds conversations and secrets",
			"dir", dir, "mode", fi.Mode().Perm().String(), "fix", "chmod 750 "+dir)
	}
}
