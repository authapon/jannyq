package llm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Embedder turns texts into vectors for similarity search.
type Embedder interface {
	// Embed returns one vector per input, in the same order. All vectors of one
	// model have the same length.
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// Embed implements Embedder with Ollama's /api/embed.
func (o *Ollama) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var resp struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/api/embed"
	// truncate: a chunk slightly over the model's window is cut rather than refused
	if err := postJSON(ctx, o.Client, url, headers, map[string]any{"model": model, "input": inputs, "truncate": true}, &resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New("llm: " + resp.Error)
	}
	return checkVectors(resp.Embeddings, len(inputs))
}

// Embed implements Embedder with the OpenAI-compatible /embeddings endpoint.
func (o *OpenAI) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var resp struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/embeddings"
	if err := postJSON(ctx, o.Client, url, headers, map[string]any{"model": model, "input": inputs}, &resp); err != nil {
		return nil, err
	}
	sort.SliceStable(resp.Data, func(i, j int) bool { return resp.Data[i].Index < resp.Data[j].Index })
	out := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		out[i] = d.Embedding
	}
	return checkVectors(out, len(inputs))
}

func checkVectors(v [][]float32, want int) ([][]float32, error) {
	if len(v) != want {
		return nil, fmt.Errorf("llm: asked for %d embeddings, got %d", want, len(v))
	}
	for _, e := range v {
		if len(e) == 0 || len(e) != len(v[0]) {
			return nil, errors.New("llm: the embedding model returned vectors of inconsistent length")
		}
	}
	return v, nil
}
