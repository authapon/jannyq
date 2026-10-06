package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/authapon/jannyq/internal/backup"
)

// envOr returns the environment variable name, or def.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("jannyq "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// runBackup: `jannyq backup` makes a backup of the data directory.
func runBackup(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("backup", stderr)
	dataDir := fs.String("data-dir", envOr("JANNYQ_DATA_DIR", "."), "data directory of the bot [JANNYQ_DATA_DIR]")
	kdb := fs.String("knowledge-db", envOr("JANNYQ_KNOWLEDGE_DB", ""), "knowledge database, if it is not <data-dir>/knowledge.db [JANNYQ_KNOWLEDGE_DB]")
	out := fs.String("out", envOr("JANNYQ_BACKUP_DIR", "backups"), "folder to write the backup into [JANNYQ_BACKUP_DIR]")
	files := fs.Bool("files", true, "include the files users sent")
	keep := fs.Int("keep", 0, "afterwards keep only this many backups in the folder (0 keeps all)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p, err := backup.Create(ctx, *out, backup.Options{DataDir: *dataDir, KnowledgeDB: *kdb, Files: *files, Version: version})
	if err != nil {
		fmt.Fprintln(stderr, "jannyq backup:", err)
		return 1
	}
	fi, _ := os.Stat(p)
	fmt.Fprintf(stdout, "backup written: %s (%d bytes)\n", p, fi.Size())
	if *keep > 0 {
		if n, err := backup.Prune(*out, *keep); err != nil {
			fmt.Fprintln(stderr, "jannyq backup: pruning:", err)
			return 1
		} else if n > 0 {
			fmt.Fprintf(stdout, "deleted %d older backup(s)\n", n)
		}
	}
	return 0
}

// runRestore: `jannyq restore --from FILE` unpacks a backup. The bot must be stopped.
func runRestore(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("restore", stderr)
	from := fs.String("from", "", "backup file to restore (required)")
	dataDir := fs.String("data-dir", envOr("JANNYQ_DATA_DIR", "."), "data directory to restore into [JANNYQ_DATA_DIR]")
	force := fs.Bool("force", false, "replace existing data (it is moved aside, not deleted)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *from == "" {
		fmt.Fprintln(stderr, "jannyq restore: --from is required")
		return 2
	}
	man, err := backup.Restore(*from, *dataDir, backup.RestoreOptions{Force: *force})
	if err != nil {
		fmt.Fprintln(stderr, "jannyq restore:", err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %d file(s) from the backup of %s into %s\n", len(man.Files), man.Created.Format("2006-01-02 15:04:05 UTC"), *dataDir)
	fmt.Fprintln(stdout, "start the bot again now; make sure that it was stopped while restoring")
	return 0
}

// runVerify: `jannyq verify FILE` checks a backup without restoring it.
func runVerify(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: jannyq verify FILE")
		return 2
	}
	man, err := backup.Verify(args[0], filepath.Dir(args[0]))
	if err != nil {
		fmt.Fprintln(stderr, "jannyq verify:", err)
		return 1
	}
	var total int64
	for _, f := range man.Files {
		total += f.Size
	}
	fmt.Fprintf(stdout, "ok: %d file(s), %d bytes, made %s by jannyq %s\n", len(man.Files), total, man.Created.Format("2006-01-02 15:04:05 UTC"), man.JannyqVer)
	return 0
}
