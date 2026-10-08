package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/knowledge"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/sandbox"
	"github.com/authapon/jannyq/internal/tool"
)

// knowledgeBase is the running shared knowledge base.
type knowledgeBase struct {
	kb    *knowledge.KB
	ix    *knowledge.Indexer
	store *knowledge.Store
}

// newEmbedder builds the client for the embedding model, or returns nil when
// no embedding model is configured (the knowledge base then searches by words).
func newEmbedder(cfg *config.Config) llm.Embedder {
	if cfg.EmbedModel == "" {
		return nil
	}
	provider := cfg.EmbedProvider
	if provider == "" {
		provider = cfg.LLMProvider
	}
	base, key := cfg.EmbedBaseURL, cfg.EmbedAPIKey
	if base == "" {
		if provider == cfg.LLMProvider {
			base = cfg.LLMBaseURL
		} else if provider == "openai" {
			base = "https://api.openai.com/v1"
		} else {
			base = "http://localhost:11434"
		}
	}
	if key == "" && provider == cfg.LLMProvider {
		key = cfg.LLMAPIKey
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	if provider == "openai" {
		return &llm.OpenAI{BaseURL: base, APIKey: key, Client: client}
	}
	return &llm.Ollama{BaseURL: base, APIKey: key, Client: client}
}

// newKnowledge opens the knowledge database and prepares the folder watcher.
// It returns nil when --knowledge-dir is not set.
func newKnowledge(ctx context.Context, cfg *config.Config, cr *commandRunner, inst metrics.Instruments, log *slog.Logger) (*knowledgeBase, error) {
	if cfg.KnowledgeDir == "" {
		return nil, nil
	}
	if fi, err := os.Stat(cfg.KnowledgeDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("--knowledge-dir %q is not a folder that can be read", cfg.KnowledgeDir)
	}
	dbPath := cfg.KnowledgeDB
	if dbPath == "" {
		dbPath = filepath.Join(cfg.DataDir, "knowledge.db")
	}
	store, err := knowledge.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open the knowledge database: %w", err)
	}
	kb := &knowledge.KB{Store: store, Embedder: newEmbedder(cfg), Model: cfg.EmbedModel, MinCosine: cfg.KnowledgeMinSim, Log: log}

	engines, ocr, _ := pdfEngines(ctx, cfg, cr, log)
	langs := cfg.OCRLanguages()
	if !ocr || cfg.KnowledgeOCRPages == 0 {
		langs = ""
	}
	proc := attach.New(attach.Config{
		MaxBytes:     int64(cfg.KnowledgeMaxFileMB) << 20,
		PDFMaxPages:  cfg.KnowledgePDFPages,
		MaxTextChars: 1 << 26,
		PageChars:    1 << 30, // a text file is one piece; only PDFs have pages
		OCRLangs:     langs,
		OCRMaxPages:  cfg.KnowledgeOCRPages,
		PageImages:   -1,
		Timeout:      20 * time.Minute,
		Concurrency:  1,
	}, log, engines...)
	ix, err := knowledge.NewIndexer(knowledge.IndexerConfig{
		Dir:          cfg.KnowledgeDir,
		Processor:    proc,
		Workspace:    sandbox.WorkspaceID("knowledge:index"),
		ChunkChars:   cfg.KnowledgeChunkChars,
		Overlap:      cfg.KnowledgeOverlap,
		Interval:     cfg.KnowledgeInterval,
		MaxFileBytes: int64(cfg.KnowledgeMaxFileMB) << 20,
		Log:          log,
		Metrics:      inst,
	}, kb)
	if err != nil {
		store.Close()
		return nil, err
	}
	log.Info("knowledge base enabled", "dir", cfg.KnowledgeDir, "db", dbPath, "embed_model", cfg.EmbedModel,
		"semantic", kb.Embedder != nil, "ocr", langs, "interval", cfg.KnowledgeInterval)
	if kb.Embedder == nil {
		log.Info("no --embed-model: the knowledge base is searched by words only")
	}
	return &knowledgeBase{kb: kb, ix: ix, store: store}, nil
}

// syncAtStart builds and updates the knowledge base before anything else runs,
// so that the bot never answers from a half-built one. It returns an error only
// when ctx ends; a folder that cannot be scanned is logged and left to the
// background scans.
func (k *knowledgeBase) syncAtStart(ctx context.Context, log *slog.Logger) error {
	log.Info("checking and building the knowledge base before the bot starts")
	start := time.Now()
	res, err := k.ix.Sync(ctx, 15*time.Second, func(p knowledge.Progress) {
		log.Info("still building the knowledge base", "files_done", p.Done, "files_to_do", p.Total)
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		log.Error("the knowledge base could not be built; the bot starts anyway and will retry in the background", "err", err)
		return nil
	}
	log.Info("knowledge base ready", "files", res.Files, "added", res.Added, "changed", res.Edited,
		"removed", res.Removed, "failed", res.Failed, "took", time.Since(start).Round(time.Millisecond))
	if res.Pending > 0 {
		log.Warn("some files are not embedded yet (is the embedding model reachable?): they are searched by words until the background scans embed them",
			"files", res.Pending)
	}
	return nil
}

// register adds the knowledge tools.
func (k *knowledgeBase) register(tools *tool.Registry, cfg *config.Config) {
	tools.Register(&tool.KnowledgeSearch{KB: k.kb, Indexer: k.ix, DefaultResults: cfg.KnowledgeResults})
	tools.Register(&tool.KnowledgeFiles{KB: k.kb})
	tools.Register(&tool.KnowledgeRead{KB: k.kb})
}
