package knowledge

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/authapon/jannyq/internal/llm"
)

// KB is the searchable knowledge base: the store plus the embedding model that
// turns questions into vectors.
type KB struct {
	Store *Store
	// Embedder embeds passages and questions; nil means the index is searched by
	// words only.
	Embedder llm.Embedder
	// Model is the embedding model's name; passages are re-embedded when it changes.
	Model string
	// MinCosine is the least similarity for a vector match to count (default 0.25).
	MinCosine float64
	// EmbedTimeout bounds the embedding of one question (default 20 s).
	EmbedTimeout time.Duration
	Log          *slog.Logger
}

// SearchResult is the outcome of a search.
type SearchResult struct {
	Hits []Hit
	// Semantic is true when the question was embedded, so meaning and not just
	// words took part.
	Semantic bool
	// Note explains degraded results (the embedding model could not be reached).
	Note string
}

// Search finds the passages that best answer query.
func (k *KB) Search(ctx context.Context, query string, limit int) (SearchResult, error) {
	var res SearchResult
	opts := SearchOptions{Limit: limit, MinCosine: k.MinCosine}
	if opts.MinCosine <= 0 {
		opts.MinCosine = 0.25
	}
	if k.Embedder != nil && k.Model != "" {
		timeout := k.EmbedTimeout
		if timeout <= 0 {
			timeout = 20 * time.Second
		}
		ectx, cancel := context.WithTimeout(ctx, timeout)
		vecs, err := k.Embedder.Embed(ectx, k.Model, []string{query})
		cancel()
		switch {
		case err == nil && len(vecs) == 1:
			opts.QueryVec, res.Semantic = vecs[0], true
		case ctx.Err() != nil:
			return res, ctx.Err()
		default:
			if k.Log != nil {
				k.Log.Warn("could not embed a question; searching by words only", "err", err)
			}
			res.Note = "semantic search is unavailable right now; the results match words only"
		}
	}
	hits, err := k.Store.Search(ctx, query, opts)
	if err != nil {
		return res, err
	}
	res.Hits = hits
	return res, nil
}

var errNoEmbedder = errors.New("knowledge: no embedding model")
