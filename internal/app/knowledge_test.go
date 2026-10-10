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
	with := func(f func(c *config.Config)) *config.Config {
		c := *base
		c.EmbedModel = "bge-m3"
		f(&c)
		return &c
	}
	ollama := func(c *config.Config) *llm.Ollama {
		o, _ := newEmbedder(c).(*llm.Ollama)
		if o == nil {
			t.Fatalf("not an Ollama client: %+v", newEmbedder(c))
		}
		return o
	}
	openai := func(c *config.Config) *llm.OpenAI {
		o, _ := newEmbedder(c).(*llm.OpenAI)
		if o == nil {
			t.Fatalf("not an OpenAI client: %+v", newEmbedder(c))
		}
		return o
	}

	// nothing given: the same server as the chat model, with its key
	if o := ollama(with(func(*config.Config) {})); o.BaseURL != "http://llm:11434" || o.APIKey != "chatkey" {
		t.Errorf("same provider reuses address and key: %+v", o)
	}
	// an address and a key of its own win
	if o := ollama(with(func(c *config.Config) { c.EmbedBaseURL, c.EmbedAPIKey = "http://emb:1", "embkey" })); o.BaseURL != "http://emb:1" || o.APIKey != "embkey" {
		t.Errorf("explicit settings win: %+v", o)
	}
	// a different address without a key of its own: the chat model's key must not be sent there
	if o := ollama(with(func(c *config.Config) { c.EmbedBaseURL = "http://emb:1" })); o.BaseURL != "http://emb:1" || o.APIKey != "" {
		t.Errorf("the chat key was sent to another server: %+v", o)
	}
	// the same address written out is the same server (a trailing slash does not matter)
	if o := ollama(with(func(c *config.Config) { c.EmbedBaseURL = "http://llm:11434/" })); o.APIKey != "chatkey" {
		t.Errorf("the chat model's own server keeps its key: %+v", o)
	}
	// only a key given: the chat model's address
	if o := ollama(with(func(c *config.Config) { c.EmbedAPIKey = "embkey" })); o.BaseURL != "http://llm:11434" || o.APIKey != "embkey" {
		t.Errorf("%+v", o)
	}
	// another provider: its own default address, and the chat model's key must not be sent to it
	if o := openai(with(func(c *config.Config) { c.EmbedModel, c.EmbedProvider = "text-embedding-3-small", "openai" })); o.BaseURL != "https://api.openai.com/v1" || o.APIKey != "" {
		t.Errorf("%+v", o)
	}
	if o := openai(with(func(c *config.Config) {
		c.EmbedProvider, c.EmbedBaseURL, c.EmbedAPIKey = "openai", "http://vllm:8000/v1", "sk-embed"
	})); o.BaseURL != "http://vllm:8000/v1" || o.APIKey != "sk-embed" {
		t.Errorf("%+v", o)
	}
	// the chat model on an OpenAI-compatible server and embeddings from Ollama: Ollama's own default address
	chat := &config.Config{LLMProvider: "openai", LLMBaseURL: "https://api.example.com/v1", LLMAPIKey: "sk-chat", EmbedModel: "bge-m3", EmbedProvider: "ollama"}
	if o := ollama(chat); o.BaseURL != "http://localhost:11434" || o.APIKey != "" {
		t.Errorf("%+v", o)
	}
	// both on OpenAI-compatible servers, embeddings elsewhere with their own key
	chat.EmbedProvider, chat.EmbedBaseURL, chat.EmbedAPIKey = "", "https://embed.example.com/v1", "sk-embed"
	if o := openai(chat); o.BaseURL != "https://embed.example.com/v1" || o.APIKey != "sk-embed" {
		t.Errorf("%+v", o)
	}
}

func TestWithoutCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"http://user:secret@emb:11434/api": "http://emb:11434/api",
		"http://emb:11434":                 "http://emb:11434",
		"://bad":                           "://bad",
	} {
		if got := withoutCredentials(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
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

func TestProviderFollowsTheThinkingSetting(t *testing.T) {
	cfg := func(provider, thinking string) *config.Config {
		return &config.Config{LLMProvider: provider, LLMBaseURL: "http://x", Thinking: thinking, ExtraBody: map[string]any{"a": 1}}
	}
	for thinking, want := range map[string]*bool{"auto": nil, "off": ptrBool(false), "on": ptrBool(true)} {
		p, err := NewProvider(cfg("ollama", thinking))
		o := p.(*llm.Ollama)
		if err != nil || (o.Think == nil) != (want == nil) || (want != nil && *o.Think != *want) || o.ExtraBody["a"] != 1 {
			t.Errorf("ollama %s: %+v %v", thinking, o, err)
		}
	}
	for thinking, want := range map[string]string{"auto": "", "on": "", "off": "none"} {
		p, err := NewProvider(cfg("openai", thinking))
		o := p.(*llm.OpenAI)
		if err != nil || o.ReasoningEffort != want || o.ExtraBody["a"] != 1 {
			t.Errorf("openai %s: %+v %v", thinking, o, err)
		}
	}
}

func ptrBool(b bool) *bool { return &b }
