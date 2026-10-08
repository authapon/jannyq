package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/tool"
)

func TestEmbedderChoice(t *testing.T) {
	base := &config.Config{LLMProvider: "ollama", LLMBaseURL: "http://llm:11434", LLMAPIKey: "chatkey"}
	if newEmbedder(base) != nil {
		t.Error("no embedding model: no embedder")
	}
	c := *base
	c.EmbedModel = "bge-m3"
	o, ok := newEmbedder(&c).(*llm.Ollama)
	if !ok || o.BaseURL != "http://llm:11434" || o.APIKey != "chatkey" {
		t.Errorf("same provider reuses address and key: %+v", newEmbedder(&c))
	}
	c.EmbedBaseURL, c.EmbedAPIKey = "http://emb:1", "embkey"
	if o, _ = newEmbedder(&c).(*llm.Ollama); o == nil || o.BaseURL != "http://emb:1" || o.APIKey != "embkey" {
		t.Errorf("explicit settings win: %+v", o)
	}
	// another provider: the chat model's key must not be sent to it
	c = *base
	c.EmbedModel, c.EmbedProvider = "text-embedding-3-small", "openai"
	oa, ok := newEmbedder(&c).(*llm.OpenAI)
	if !ok || oa.BaseURL != "https://api.openai.com/v1" || oa.APIKey != "" {
		t.Errorf("%+v", newEmbedder(&c))
	}
}

func TestNewKnowledge(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	cfg := &config.Config{DataDir: data, KnowledgeDir: dir, PDFEngine: "native", OCRLangs: "auto", Lang: "en",
		KnowledgeMaxFileMB: 5, KnowledgePDFPages: 100, KnowledgeChunkChars: 500, KnowledgeOverlap: 50, KnowledgeResults: 3, KnowledgeInterval: 1e9}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("Pangolins are the most trafficked mammals in the world."), 0o644); err != nil {
		t.Fatal(err)
	}
	old := timeAgo()
	_ = os.Chtimes(filepath.Join(dir, "a.txt"), old, old)
	k, err := newKnowledge(context.Background(), cfg, nil, metrics.NewInstruments(nil), quiet())
	if err != nil || k == nil {
		t.Fatalf("%v", err)
	}
	defer k.store.Close()
	if _, err := os.Stat(filepath.Join(data, "knowledge.db")); err != nil {
		t.Errorf("the database is not in the data directory: %v", err)
	}
	if _, err := k.ix.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewRegistry()
	k.register(reg, cfg)
	search, ok := reg.Get("knowledge_search")
	if !ok {
		t.Fatal("tool not registered")
	}
	if _, ok := reg.Get("knowledge_files"); !ok {
		t.Fatal("files tool not registered")
	}
	out, err := search.Execute(context.Background(), tool.CallContext{}, []byte(`{"query":"pangolins"}`))
	if err != nil || !strings.Contains(out, "a.txt") || !strings.Contains(out, "most trafficked") {
		t.Errorf("%v\n%s", err, out)
	}
	// off without a folder; an error for a folder that does not exist
	if k2, err := newKnowledge(context.Background(), &config.Config{}, nil, metrics.NewInstruments(nil), quiet()); k2 != nil || err != nil {
		t.Errorf("disabled: %v %v", k2, err)
	}
	cfg.KnowledgeDir = filepath.Join(dir, "missing")
	if _, err := newKnowledge(context.Background(), cfg, nil, metrics.NewInstruments(nil), quiet()); err == nil {
		t.Error("a missing folder must stop the start-up with a clear error")
	}
}

func timeAgo() time.Time { return time.Now().Add(-time.Hour) }

func TestSyncAtStartBuildsTheWholeKnowledgeBase(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	cfg := &config.Config{DataDir: data, KnowledgeDir: dir, PDFEngine: "native", OCRLangs: "auto", Lang: "en",
		KnowledgeMaxFileMB: 5, KnowledgePDFPages: 100, KnowledgeChunkChars: 500, KnowledgeOverlap: 50, KnowledgeResults: 3, KnowledgeInterval: 1e9}
	for i, text := range []string{"Pangolins are the most trafficked mammals in the world.", "Axolotls regrow lost limbs.", "ไฟล์ภาษาไทยเกี่ยวกับช้างและป่า"} {
		p := filepath.Join(dir, string(rune('a'+i))+".txt")
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		old := timeAgo()
		_ = os.Chtimes(p, old, old)
	}
	k, err := newKnowledge(context.Background(), cfg, nil, metrics.NewInstruments(nil), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer k.store.Close()
	if err := k.syncAtStart(context.Background(), quiet()); err != nil {
		t.Fatal(err)
	}
	// no further scan is needed: the files are all there already
	for _, q := range []string{"pangolins", "axolotls", "ช้าง"} {
		res, err := k.kb.Search(context.Background(), q, 3)
		if err != nil || len(res.Hits) == 0 {
			t.Errorf("%q: %+v, err %v", q, res, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := k.syncAtStart(ctx, quiet()); err == nil {
		t.Error("an interrupted start-up must say so")
	}
}
