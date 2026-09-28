package server

// The usage path end to end inside one gateway: a request's record published to the
// control client's usage batch, the batch sealed, sent and acknowledged by the
// control plane (fakecontrol), and the limiter settling the request against the
// totals the ack carries.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/fakebackend"
	"kaiak/internal/fakecontrol"
	"kaiak/internal/limits"
	"kaiak/internal/metrics"
	"kaiak/internal/provider"
	"kaiak/internal/routing"
	"kaiak/internal/state"
)

// controlledGateway is a test gateway in control-plane mode: a shared limiter fed by
// a control client that sends usage to cp.
type controlledGateway struct {
	*testGateway
	cp     *fakecontrol.Server
	client *control.Client
	// acks receives every totals update the client hands the limiter.
	acks chan control.TotalsUpdate
}

const controlToken = "server-test-token"

// newControlledGateway builds the gateway with the test config (limits edited in by
// edit), a control client sealing a batch every maxRecords records, and runs the
// client until the test ends.
func newControlledGateway(t *testing.T, maxRecords int, edit func(doc string) string) *controlledGateway {
	t.Helper()
	backend := fakebackend.New()
	t.Cleanup(backend.Close)
	cp := fakecontrol.New(controlToken)
	t.Cleanup(cp.Close)
	doc := testDocWith(backend.URL(), edit)
	cp.Publish([]byte(doc))

	holder := &config.Holder{}
	snap := parseTestDoc(t, doc)
	snap.Version = config.Version{Epoch: cp.ConfigEpoch(), Number: 1} // the published version
	holder.Swap(snap)
	env := map[string]string{"LOCAL_KEY": localBackendKey, "AZURE_KEY": azureBackendKey}
	lookupEnv := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	var logs bytes.Buffer
	log := &lockedWriter{w: &logs}
	logger := slog.New(slog.NewJSONHandler(log, nil))
	dir, err := state.Open(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(cp.URL())

	var client *control.Client
	limiter := limits.NewShared(holder, time.Now, func() limits.Contact {
		connected, last := client.Contact()
		return limits.Contact{Connected: connected, Last: last, UsageWaitingSince: client.UsageWaitingSince()}
	}, nil)
	acks := make(chan control.TotalsUpdate, 64)
	// The client's own config follower applies into a holder of its own: this
	// gateway serves the test config, the client only carries usage.
	applier := config.NewApplier(&config.Holder{}, logger, lookupEnv, nil)
	client = control.New(control.Options{URL: u, Token: controlToken, Instance: "gw-test", Applier: applier,
		Dir: dir, Logger: logger, BatchInterval: time.Hour, BatchMaxRecords: maxRecords,
		BackoffBase: 10 * time.Millisecond, BackoffCap: 50 * time.Millisecond,
		OnTotals: func(up control.TotalsUpdate) {
			limiter.TakeTotals(testLimitsTotals(up.Totals), up.Counted)
			acks <- up
		}})
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Go(func() { client.Run(ctx) })
	t.Cleanup(func() { cancel(); running.Wait() })

	providers := provider.NewRegistry(lookupEnv)
	usage := &usageSink{settled: make(chan accounting.UsageRecord, 64)}
	reg := metrics.NewRegistry()
	router := routing.New(routing.Options{Probe: providers.Probe, Observer: metrics.NewCircuits(reg), Logger: logger})
	usageMetrics := metrics.NewUsageSink(reg, holder)
	recorder := accounting.NewRecorder(accounting.RecorderOptions{Instance: "gw-test",
		Sink: accounting.Fanout{usage, usageMetrics}, Batcher: client, Logger: logger,
		OutOfRange: usageMetrics.RecordClamped})
	drain := NewDrain()
	h := NewAPI(holder, drain, NewBodyBudget(DefaultBodyMemory), providers, limiter, router, recorder, metrics.NewOps(reg, router, holder), logger)
	g := &testGateway{h: h, logs: &logs, log: log, backend: backend, holder: holder, router: router,
		limiter: limiter, usage: usage, metrics: reg, drain: drain}
	return &controlledGateway{testGateway: g, cp: cp, client: client, acks: acks}
}

// testLimitsTotals converts the control plane's totals for the limiter, as the
// binary's wiring does.
func testLimitsTotals(t *control.Totals) *limits.Totals {
	if t == nil {
		return nil
	}
	out := &limits.Totals{Config: config.Version{Epoch: t.ConfigEpoch, Number: t.ConfigVersion},
		LiveGateways: t.LiveGateways}
	for _, w := range t.Windows {
		out.Windows = append(out.Windows, limits.PushedWindow{Group: w.Group, Type: w.Type,
			Models: w.Models, Start: w.WindowStart, Used: w.Used})
	}
	return out
}

// nextCounted waits for a totals update that shows a batch counted.
func (g *controlledGateway) nextCounted(t *testing.T) control.TotalsUpdate {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case up := <-g.acks:
			if up.Counted != 0 {
				return up
			}
		case <-timeout:
			t.Fatal("no batch acknowledged")
		}
	}
}

// hourWindow is a totals window of the workload eval's tokens_per_hour limit in the
// current hour, used tokens.
func hourWindow(used int64) []byte {
	start := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	return fmt.Appendf(nil, `[{"group":"eval","type":"tokens_per_hour","window_start":%q,"used":"%d"}]`,
		start, used)
}

func workloadLimits(limitsJSON string) func(doc string) string {
	return func(doc string) string {
		return strings.Replace(doc, `"allowed_models": ["*"] }`, `"allowed_models": ["*"], "limits": `+limitsJSON+` }`, 1)
	}
}

// H4: the record that fills a batch seals it as the request finishes. Its usage is
// counted by the control plane (the ack's totals hold it) and must leave the
// gateway's own count in the same step — no later traffic comes to clear it.
func TestRecordFillingABatchIsCountedOnce(t *testing.T) {
	g := newControlledGateway(t, 1, workloadLimits(`[{ "type": "tokens_per_hour", "value": 1000 }]`))
	g.backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 590, CompletionTokens: 10}})
	// The control plane's total after counting the one batch: its 600 tokens.
	g.cp.SetWindows(hourWindow(600))

	w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody})
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	g.nextCounted(t)
	if got := counterUsed(t, g.testGateway, "eval", config.LimitTokensPerHour); got != 600 {
		t.Errorf("hour used %d after the ack, want the control plane's 600 and nothing of its own", got)
	}
	// 400 tokens are left: a request reserving 20 fits.
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != 200 {
		t.Errorf("next request: status %d: %s", w.Code, w.Body.String())
	}
}

// H5: a backend reporting 2^53 tokens. The record is clamped to the protocol's bound
// at settlement, so its batch — and the other records in it — is accepted: every
// counted record passes the usage record's checks.
func TestBackendReportingTooManyTokensDoesNotLoseItsBatch(t *testing.T) {
	g := newControlledGateway(t, 2, nil)
	g.backend.QueueReplies(fakebackend.Reply{}, fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 1 << 53}})
	for i := range 2 {
		if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != 200 {
			t.Fatalf("request %d: status %d: %s", i+1, w.Code, w.Body.String())
		}
	}
	g.nextCounted(t)
	counted := g.cp.CountedRecords()
	if len(counted) != 2 {
		t.Fatalf("%d records counted, want both", len(counted))
	}
	for _, raw := range counted {
		rec, err := control.DecodeUsageRecord(raw)
		if err != nil {
			t.Errorf("counted record breaks the protocol: %v\n%s", err, raw)
			continue
		}
		if rec.Units[config.UnitTokensIn] > accounting.MaxAmount {
			t.Errorf("tokens_in %d above the bound", rec.Units[config.UnitTokensIn])
		}
	}
	expectMetricLines(t, g.metricsText(), `kaiak_usage_clamped_records_total 1`)
	if !strings.Contains(g.logText(), "usage out of the protocol") {
		t.Errorf("clamp not logged:\n%s", g.logText())
	}
}

// M16: the config stream stays up while /v1/usage keeps failing. The stream alone is
// no longer contact for money limits: with batches waiting for an ack past the
// outage grace, a priced model under a USD limit is refused as in an outage (free
// models keep serving); the first ack ends it.
func TestUsageAcksFailingPastTheGraceRefusePricedBudgets(t *testing.T) {
	g := newControlledGateway(t, 1, func(doc string) string {
		doc = strings.Replace(doc, `"max_request_body_bytes": 1024 }`, `"max_request_body_bytes": 1024, "control_outage_grace_ms": 300 }`, 1)
		return strings.Replace(doc, `"research": {}`, `"research": { "limits": [{ "type": "usd_per_month", "value": 100 }] }`, 1)
	})
	pair := call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: `{"model":"pair","messages":[]}`}
	waitFor(t, func() bool { connected, _ := g.client.Contact(); return connected })
	// The totals that follow the stream's replay make the budget's spend known (D8).
	select {
	case <-g.limiter.FirstTotals():
	case <-time.After(10 * time.Second):
		t.Fatal("no totals after the stream connected")
	}
	g.cp.SetUsageFault(&fakecontrol.UsageFault{Status: 500, Code: "internal-error"})

	if w := do(t, g.h, pair); w.Code != 200 {
		t.Fatalf("first request: status %d: %s", w.Code, w.Body.String())
	}
	time.Sleep(500 * time.Millisecond)
	if connected, _ := g.client.Contact(); !connected {
		t.Fatal("the config stream closed; the test needs it open")
	}
	expectError(t, do(t, g.h, pair), 503, "budget_unavailable")
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != 200 {
		t.Errorf("unpriced model: status %d: %s", w.Code, w.Body.String())
	}

	g.cp.SetUsageFault(nil)
	g.nextCounted(t)
	if w := do(t, g.h, pair); w.Code != 200 {
		t.Errorf("after the ack: status %d: %s", w.Code, w.Body.String())
	}
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
