package router

import (
	"strings"
	"testing"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/metrics"
)

func TestRouterMetrics(t *testing.T) {
	reg := metrics.New()
	inst := metrics.NewInstruments(reg)
	f := &fakeLLM{}
	r := newRouterWith(t, f, Config{AllowedUsers: []string{"u1"}, RateLimit: 2, Metrics: inst}, agent.Config{Model: "m", CompactAfter: 200, CompactKeep: 20, Metrics: inst})
	rec := &recorder{}

	r.Handle(ctx, msg(rec, "one"))
	r.Handle(ctx, msg(rec, "two"))
	r.Handle(ctx, msg(rec, "three")) // over the rate limit of 2
	stranger := msg(rec, "let me in")
	stranger.UserID = "u9"
	r.Handle(ctx, stranger)
	chatter := msg(rec, "just people talking")
	chatter.IsGroup, chatter.Addressed = true, false
	r.Handle(ctx, chatter)

	out := reg.Render()
	for _, want := range []string{
		`jannyq_messages_received_total{channel="test",addressed="true"} 4`,
		`jannyq_messages_received_total{channel="test",addressed="false"} 1`,
		`jannyq_messages_rejected_total{channel="test",reason="rate_limited"} 1`,
		`jannyq_messages_rejected_total{channel="test",reason="not_allowed"} 1`,
		`jannyq_replies_total{channel="test",result="ok"} 2`,
		`jannyq_request_duration_seconds_count{channel="test"} 2`,
		`jannyq_llm_requests_total{result="ok"} 2`,
		`jannyq_llm_request_duration_seconds_count 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}
