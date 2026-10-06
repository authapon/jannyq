package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounterGaugeHistogramRendering(t *testing.T) {
	r := New()
	c := r.Counter("jq_things_total", "Things that happened.", "kind", "result")
	c.Inc("a", "ok")
	c.Add(2, "a", "ok")
	c.Inc("b", `we"ird\va
lue`)
	plain := r.Counter("jq_plain_total", "No labels.")
	r.GaugeFunc("jq_level", "A level.", []string{"pool"}, func() []Sample {
		return []Sample{{Labels: []string{"z"}, Value: 2.5}, {Labels: []string{"a"}, Value: 1}}
	})
	h := r.Histogram("jq_seconds", "Durations.", []float64{1, 5}, "ch")
	h.Observe(0.5, "x")
	h.Observe(3, "x")
	h.Observe(9, "x")
	out := r.Render()
	for _, want := range []string{
		"# HELP jq_things_total Things that happened.\n# TYPE jq_things_total counter\n",
		`jq_things_total{kind="a",result="ok"} 3` + "\n",
		`jq_things_total{kind="b",result="we\"ird\\va\nlue"} 1` + "\n",
		"jq_plain_total 0\n",
		"# TYPE jq_level gauge\n" + `jq_level{pool="a"} 1` + "\n" + `jq_level{pool="z"} 2.5` + "\n",
		`jq_seconds_bucket{ch="x",le="1"} 1` + "\n",
		`jq_seconds_bucket{ch="x",le="5"} 2` + "\n",
		`jq_seconds_bucket{ch="x",le="+Inf"} 3` + "\n",
		`jq_seconds_sum{ch="x"} 12.5` + "\n",
		`jq_seconds_count{ch="x"} 3` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	plain.Inc()
	if !strings.Contains(r.Render(), "jq_plain_total 1\n") {
		t.Error("a counter without labels")
	}
	// the output is stable
	if r.Render() != r.Render() {
		t.Error("two renderings differ")
	}
}

func TestNilRegistryAndMetricsDoNothing(t *testing.T) {
	var r *Registry
	c := r.Counter("x_total", "x", "a")
	c.Inc("1")
	r.Histogram("h", "h", nil).Observe(1)
	r.GaugeFunc("g", "g", nil, nil)
	r.RegisterRuntime("v")
	var h *Histogram
	h.Since(time.Now())
	if r.Render() != "" {
		t.Error("a nil registry rendered something")
	}
	in := NewInstruments(nil)
	in.MessagesReceived.Inc("telegram", "true") // must not panic
	in.RequestSeconds.Observe(1, "telegram")
}

func TestWrongLabelCountAndNegativeAddsAreIgnored(t *testing.T) {
	r := New()
	c := r.Counter("c_total", "c", "a")
	c.Inc()
	c.Inc("1", "2")
	c.Add(-5, "1")
	c.Inc("ok")
	out := r.Render()
	if strings.Count(out, "c_total{") != 1 || !strings.Contains(out, `c_total{a="ok"} 1`) {
		t.Errorf("%s", out)
	}
}

func TestSeriesAreBounded(t *testing.T) {
	r := New()
	c := r.Counter("c_total", "c", "id")
	for i := 0; i < maxSeries+50; i++ {
		c.Inc(string(rune('a'+i%26)) + strings.Repeat("x", i))
	}
	out := r.Render()
	if n := strings.Count(out, "c_total{"); n != maxSeries {
		t.Errorf("%d series", n)
	}
	if !strings.Contains(out, "c_total_dropped_total 50") {
		t.Errorf("drops are not reported:\n%s", out[len(out)-200:])
	}
	h := r.Histogram("h", "h", nil, "id")
	for i := 0; i < maxSeries+10; i++ {
		h.Observe(1, strings.Repeat("y", i+1))
	}
	if n := strings.Count(r.Render(), "h_count{"); n != maxSeries {
		t.Errorf("%d histogram series", n)
	}
}

func TestDuplicateNamesPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a duplicate metric name was accepted")
		}
	}()
	r := New()
	r.Counter("same_total", "a")
	r.Counter("same_total", "b")
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	c := r.Counter("c_total", "c", "k")
	h := r.Histogram("h", "h", nil, "k")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				c.Inc("a")
				h.Observe(0.1, "a")
				_ = r.Render()
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(r.Render(), `c_total{k="a"} 8000`) {
		t.Error("lost increments")
	}
}

func TestHandler(t *testing.T) {
	r := New()
	r.Counter("c_total", "c").Inc()
	r.RegisterRuntime("1.2.3")
	do := func(h http.Handler, method, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/metrics", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	open := do(r.Handler(""), "GET", "")
	if open.Code != 200 || !strings.HasPrefix(open.Header().Get("Content-Type"), "text/plain; version=0.0.4") ||
		!strings.Contains(open.Body.String(), "c_total 1") || !strings.Contains(open.Body.String(), `jannyq_build_info{version="1.2.3"`) ||
		!strings.Contains(open.Body.String(), "go_goroutines") {
		t.Errorf("%d %s", open.Code, open.Body)
	}
	guarded := r.Handler("sekret-token")
	for name, auth := range map[string]string{"none": "", "wrong": "Bearer nope", "no scheme": "sekret-token-x"} {
		if rec := do(guarded, "GET", auth); rec.Code != 401 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec := do(guarded, "GET", "Bearer sekret-token"); rec.Code != 200 {
		t.Errorf("with the token: %d", rec.Code)
	}
	if rec := do(r.Handler(""), "POST", ""); rec.Code != 405 {
		t.Errorf("POST: %d", rec.Code)
	}
}
