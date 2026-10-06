//go:build crosshalf

package e2e

// The cross-half end-to-end test: the real sample control plane (Node, from
// control/sample) and two kaiak processes, all behind the fake backend. Built only
// with -tags crosshalf, so the gateway's own checks never need Node;
// scripts/check-all.sh runs it. The sample's state is read from its totals stream, the
// gateways' from their logs, metrics and response headers.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/fakebackend"
	"kaiak/internal/sse"
)

func TestAcrossHalves(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	node := requireSample(t, root)

	backend := fakebackend.New()
	defer backend.Close()
	// Answers every request with a 400 and no usage: requests routed to it spend
	// nothing, so they show a gateway's budget decision without changing any total.
	refuser := fakebackend.New()
	defer refuser.Close()
	refuser.SetReply(fakebackend.Reply{Status: http.StatusBadRequest})
	// A closed port: connections to it are refused.
	gone := fakebackend.New()
	downURL := gone.URL()
	gone.Close()
	// One request at a time ("capped", max_in_flight 1): a held stream makes the
	// next request queue.
	capped := fakebackend.New()
	defer capped.Close()

	const token = "e2e-cross-half-token"
	dir := t.TempDir()
	// The sample's config starts as a projected ConfigMap lays it out; a later subtest
	// edits it in place instead, as a local run does.
	configMap := newProjectedDir(t, filepath.Join(dir, "configmap"), "sample-config.json")
	configFile := configMap.file
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	cfg := crossHalfConfig(backend.URL(), refuser.URL(), downURL, capped.URL(), evalHash, annHash)
	configMap.project(t, cfg)

	sample := startSample(t, node, root, configFile, token)
	proxy := newControlProxy(t)
	proxy.setUpstream(t, sample.url)

	dataA := filepath.Join(dir, "gw-a")
	gatewayEnv := func(instance, dataDir string, extra ...string) []string {
		env := append(controlEnv(proxy.URL(), token, dataDir), "KAIAK_INSTANCE_ID="+instance,
			// Bounds the drain's flush when the control plane is down.
			"KAIAK_DRAIN_TIMEOUT_MS=2000")
		return append(env, extra...)
	}
	a := startGatewayEnv(t, gatewayEnv("gw-a", dataA))
	b := startGatewayEnv(t, gatewayEnv("gw-b", filepath.Join(dir, "gw-b")))
	for _, g := range []*gateway{a, b} {
		g.logs.wait(t, "the boot from the sample", msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "1"))
	}

	// tokens holds what the gateways served since the sample's store began: every
	// answer's tokens a token limit counts — its total less the input read from the
	// cache — which the sample's hourly token total must equal once counted.
	var tokens servedTokens
	served := func(t *testing.T, what string, r *response) {
		t.Helper()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", what, r.StatusCode, r.body)
		}
		var body struct {
			Usage answerUsage `json:"usage"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil || body.Usage.Total <= 0 {
			t.Fatalf("%s: no usage in %s (%v)", what, r.body, err)
		}
		tokens.add(body.Usage.limitTokens())
	}
	chat := func(t *testing.T, g *gateway, model string) *response {
		t.Helper()
		return g.post(t, "/v1/chat/completions", evalKey, "", chatBody(model, false, nil))
	}
	allCounted := func(t *testing.T, limit time.Duration) {
		t.Helper()
		sample.totals.wait(t, fmt.Sprintf("the hourly token total at %d", tokens.total()), limit, tokens.counted)
	}

	t.Run("the per-minute limit is split between the two gateways", func(t *testing.T) {
		// eval allows 1000 requests a minute: each gateway's share shows in the header
		// once the sample has pushed two live gateways.
		for _, g := range []*gateway{a, b} {
			pollUntil(t, "a share of 500 requests a minute", waitLimit, func() bool {
				r := chat(t, g, "chat")
				served(t, "chat", r)
				return r.Header.Get("x-ratelimit-limit-requests") == "500"
			})
		}
		// metered allows 2: one each.
		rpm := func(g *gateway) *response {
			return g.post(t, "/v1/chat/completions", rpmKey, "", chatBody("rpm", false, nil))
		}
		for _, g := range []*gateway{a, b} {
			r := rpm(g)
			served(t, "rpm", r)
			if r.Header.Get("x-ratelimit-limit-requests") != "1" || r.Header.Get("x-ratelimit-remaining-requests") != "0" {
				t.Fatalf("rpm: limit %q remaining %q, want the share 1 and 0 left",
					r.Header.Get("x-ratelimit-limit-requests"), r.Header.Get("x-ratelimit-remaining-requests"))
			}
			if r := rpm(g); r.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("second rpm request: %d %s, want 429 on the share", r.StatusCode, r.body)
			}
		}
		sample.totals.wait(t, "two live gateways", waitLimit, func(tot control.Totals) bool { return tot.LiveGateways == 2 })
	})

	t.Run("a projected ConfigMap update reaches both gateways", func(t *testing.T) {
		// Kubernetes swaps the ..data symlink to a new directory: config.json itself
		// never changes, only what its link resolves to.
		cfg["models"].(map[string]any)["chat-cm"] = cfg["models"].(map[string]any)["rpm"]
		configMap.project(t, cfg)
		for _, g := range []*gateway{a, b} {
			g.logs.wait(t, "the swapped config", msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "2"))
			if status, body := g.get(t, "/v1/models/chat-cm", evalKey); status != http.StatusOK {
				t.Fatalf("/v1/models/chat-cm after the swap = %d %s", status, body)
			}
		}
	})

	t.Run("an edit of the config file reaches both gateways", func(t *testing.T) {
		// Saved over the link, as an editor does: a plain file from here on.
		cfg["models"].(map[string]any)["chat-2"] = cfg["models"].(map[string]any)["rpm"]
		writeConfigFile(t, configFile, cfg)
		for _, g := range []*gateway{a, b} {
			g.logs.wait(t, "the pushed edit", msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "3"))
			if status, body := g.get(t, "/v1/models/chat-2", evalKey); status != http.StatusOK {
				t.Fatalf("/v1/models/chat-2 after the edit = %d %s", status, body)
			}
		}
	})

	t.Run("usage from both gateways adds up in the shared totals", func(t *testing.T) {
		served(t, "chat-2 on gw-a", chat(t, a, "chat-2"))
		served(t, "chat-2 on gw-b", chat(t, b, "chat-2"))
		allCounted(t, waitLimit)
		for _, g := range []*gateway{a, b} {
			if got := g.metric(t, `kaiak_usage_batch_sends_total{result="acked"}`); got < 1 {
				t.Errorf("acked batches = %v, want some from each gateway", got)
			}
		}
	})

	t.Run("input written to the cache counts toward the token total, input read from it not", func(t *testing.T) {
		// 2036 prompt tokens (3 plain, 1024 read, 1009 written) and 40 out: of the
		// answer's total_tokens, 2076, the hourly token limit counts 1052.
		backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{
			PromptTokens: 2036, CompletionTokens: 40, CachedTokens: 1024, CacheWriteTokens: 1009}})
		defer backend.SetReply(fakebackend.Reply{})
		served(t, "chat on gw-a, input written to the cache", chat(t, a, "chat"))
		allCounted(t, waitLimit)
	})

	t.Run("Messages and Responses answers settle at the sample; token counting leaves no record", func(t *testing.T) {
		// 100 prompt tokens (30 plain, 40 read from the cache, 30 written to it) and 5
		// out, reported in each API's own shape: every record carries the same units,
		// and the hourly token limit counts 65 of each (input read from the cache
		// left out).
		backend.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{
			PromptTokens: 100, CompletionTokens: 5, CachedTokens: 40, CacheWriteTokens: 30}})
		defer backend.SetReply(fakebackend.Reply{})
		want := accounting.Units{config.UnitTokensIn: 30, config.UnitTokensCached: 40, config.UnitTokensCacheWrite: 30,
			config.UnitTokensOut: 5, config.UnitTokensReasoning: 0}
		const limitTokens = 65

		// Counted first, on the gateway whose answers follow: a batch holding a later
		// answer's record would hold theirs too.
		counts := map[string]*response{
			"xh-count-messages": a.postMessages(t, "/v1/messages/count_tokens", evalKey, "xh-count-messages",
				map[string]any{"model": "agent", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}),
			"xh-count-responses": b.post(t, "/v1/responses/input_tokens", evalKey, "xh-count-responses",
				map[string]any{"model": "agent", "input": "hi"}),
		}
		for id, r := range counts {
			if r.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
			}
		}

		var answers []string
		for _, stream := range []bool{false, true} {
			id := fmt.Sprintf("xh-messages-stream-%v", stream)
			r := a.postMessages(t, "/v1/messages", evalKey, id, map[string]any{"model": "agent", "max_tokens": 32,
				"stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			if r.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
			}
			tokens.add(limitTokens)
			answers = append(answers, id)

			id = fmt.Sprintf("xh-responses-stream-%v", stream)
			r = b.post(t, "/v1/responses", evalKey, id, map[string]any{"model": "agent", "stream": stream, "input": "hi"})
			if r.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
			}
			tokens.add(limitTokens)
			answers = append(answers, id)
		}

		for _, id := range answers {
			record := proxy.waitRecord(t, id, waitLimit)
			if !maps.Equal(record.Units, want) || record.Model != "agent" || record.Deployment.Backend != "ls" ||
				record.Estimated || record.Partial {
				t.Errorf("%s: record %+v, want units %v on ls, reported", id, record, want)
			}
		}
		for id := range counts {
			if record, ok := proxy.record(id); ok {
				t.Errorf("%s: a token-counting request left a record: %+v", id, record)
			}
		}
		allCounted(t, waitLimit)
	})

	t.Run("a group's budget spent through one gateway is enforced on the other", func(t *testing.T) {
		if r := chat(t, b, "priced-probe"); r.StatusCode != http.StatusBadRequest {
			t.Fatalf("probe on gw-b before the spend: %d %s, want the backend's 400", r.StatusCode, r.body)
		}
		served(t, "priced on gw-a", chat(t, a, "priced")) // one answer is past the $0.0001 budget
		// 7 tokens in at $10 and 4 out at $20 per million: 150000 nano-USD, counted
		// toward the team research, an ancestor of the key's group.
		sample.totals.wait(t, "the spend counted", waitLimit, func(tot control.Totals) bool {
			return used(tot, "research", config.LimitUSDPerMonth) == 150_000
		})
		pollUntil(t, "gw-b refusing the budget's models", waitLimit, func() bool {
			r := chat(t, b, "priced-probe")
			switch {
			case r.StatusCode == http.StatusTooManyRequests && r.errorCode(t) == "budget_exceeded":
				return true
			case r.StatusCode != http.StatusBadRequest:
				t.Fatalf("probe on gw-b: %d %s", r.StatusCode, r.body)
			}
			return false
		})
		served(t, "chat on gw-b, outside the budget", chat(t, b, "chat"))
	})

	t.Run("a lost usage ack is not counted twice", func(t *testing.T) {
		acked := a.metric(t, `kaiak_usage_batch_sends_total{result="acked"}`)
		proxy.dropNextUsageAnswer("gw-a")
		served(t, "chat on gw-a", chat(t, a, "chat"))
		select {
		case status := <-proxy.dropped:
			if status != http.StatusOK {
				t.Fatalf("the dropped usage answer was %d, want 200 (counted)", status)
			}
		case <-time.After(waitLimit):
			t.Fatal("gw-a sent no usage batch")
		}
		a.waitMetric(t, "the resent batch acknowledged", `kaiak_usage_batch_sends_total{result="acked"}`,
			func(v float64) bool { return v > acked })
		if got := a.metric(t, `kaiak_usage_batch_sends_total{result="failed"}`); got < 1 {
			t.Errorf("failed sends = %v, want the lost answer counted", got)
		}
		// A batch counted after the resend: totals that include it include whatever
		// the resend did, so a double count cannot hide behind a push not yet sent.
		served(t, "chat on gw-b", chat(t, b, "chat"))
		allCounted(t, waitLimit)
	})

	t.Run("the sample receives an open circuit and a queued model in a status", func(t *testing.T) {
		// "down" refuses connections: the first failure opens its circuit (threshold
		// 1), and the one deployment left for the retry is the open one.
		if r := chat(t, a, "down"); r.StatusCode != http.StatusBadGateway || r.errorCode(t) != "upstream_unavailable" {
			t.Fatalf("down: %d %s, want 502 upstream_unavailable", r.StatusCode, r.body)
		}
		a.logs.wait(t, "the circuit opening", msg("circuit opened", "kaiak.backend.id", "down"))

		pace := make(chan struct{})
		capped.SetReply(fakebackend.Reply{Pace: pace})
		stream := openStream(t, a, evalKey, "held-stream",
			chatBody("held", true, map[string]any{"stream_options": map[string]any{"include_usage": true}}))
		waiting := postAsync(a, evalKey, "held-queued", "held")
		a.waitMetric(t, "the request queued", `kaiak_queued_requests{model="held"}`, func(v float64) bool { return v == 1 })
		downOpen := func(st control.Status) bool {
			d := st.Backends["down"].Deployments[backendChatModel]
			return d.Circuit == control.CircuitOpen && d.OpenedAt != nil
		}
		// The queue starting sends a report within the status minimum gap.
		st := proxy.waitStatus(t, "the open circuit and the queued request", "gw-a", 0, waitLimit, func(st control.Status) bool {
			return downOpen(st) && st.Models["held"].Queued == 1
		})
		if c := st.Backends["capped"]; c.InFlight != 1 || c.MaxInFlight != 1 {
			t.Errorf("capped backend in the report: %+v, want 1 in flight of 1", c)
		}
		if d := st.Backends["fake"].Deployments[backendChatModel]; d.Circuit != control.CircuitClosed {
			t.Errorf("the working backend's deployment in the report: %+v, want closed", d)
		}

		mark := proxy.statusMark()
		close(pace)
		evs := finishStream(t, stream)
		var usage struct {
			Usage answerUsage `json:"usage"`
		}
		if len(evs) < 2 || json.Unmarshal([]byte(evs[len(evs)-2]), &usage) != nil || usage.Usage.Total <= 0 {
			t.Fatalf("held stream: no usage chunk in %q", evs)
		}
		tokens.add(usage.Usage.limitTokens())
		res := await(t, waiting)
		served(t, "the queued request", &response{Response: &http.Response{StatusCode: res.status}, body: res.body})
		// The queue ending sends a report too; the circuit stays open (no probe
		// succeeds against a closed port).
		proxy.waitStatus(t, "the queue emptied", "gw-a", mark, waitLimit, func(st control.Status) bool {
			return downOpen(st) && st.Models["held"].Queued == 0
		})
		allCounted(t, waitLimit)
	})

	// Every batch acknowledged before the sample stops: a batch counted by this sample
	// and resent to the next would count in both stores.
	for _, g := range []*gateway{a, b} {
		g.waitMetric(t, "an empty spool", "kaiak_usage_spool_batches", func(v float64) bool { return v == 0 })
	}
	sample.stop(t)
	proxy.setUpstream(t, "")
	tokens = servedTokens{} // the next sample starts with an empty store

	t.Run("with the control plane gone money-limited models fail closed, others serve", func(t *testing.T) {
		for _, g := range []*gateway{a, b} {
			g.waitMetric(t, "the outage past the grace", "kaiak_control_outage", func(v float64) bool { return v == 1 })
			r := chat(t, g, "priced-probe")
			if r.StatusCode != http.StatusServiceUnavailable || r.errorCode(t) != "budget_unavailable" {
				t.Fatalf("money-limited model in the outage: %d %s, want 503 budget_unavailable", r.StatusCode, r.body)
			}
			served(t, "chat in the outage", chat(t, g, "chat"))
		}
	})

	// Restarted in the outage. The drain sealed the outage's records; with nowhere to
	// send them they stay in the spool.
	a.stop(t)
	a.logs.wait(t, "the failed flush", msg("usage not flushed: left in the spool for the next start"))
	a = startGatewayEnv(t, gatewayEnv("gw-a", dataA, "KAIAK_CONTROL_BOOT_WAIT_MS=500"))

	t.Run("a gateway restarted in the outage boots from last-known-good", func(t *testing.T) {
		a.logs.wait(t, "the last-known-good boot", msg("config applied", "kaiak.trigger", "last-known-good", "kaiak.config.version", "3"))
		a.logs.wait(t, "the restored spool", msg("usage spool restored"))
		served(t, "chat-2 from last-known-good", chat(t, a, "chat-2"))
	})

	sample = startSample(t, node, root, configFile, token)
	proxy.setUpstream(t, sample.url)

	t.Run("the control plane back: resynced, serving, spool delivered", func(t *testing.T) {
		// The new sample counts versions from 1 again: both gateways take its snapshot
		// though they ran version 2.
		a.logs.waitCount(t, "the resync", 1, recoverLimit, msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "1"))
		b.logs.waitCount(t, "the resync", 2, recoverLimit, msg("config applied", "kaiak.trigger", "control", "kaiak.config.version", "1"))
		for _, g := range []*gateway{a, b} {
			g.waitMetricWithin(t, "the outage over", "kaiak_control_outage", recoverLimit, func(v float64) bool { return v == 0 })
			g.waitMetricWithin(t, "the spool delivered", "kaiak_usage_spool_batches", recoverLimit, func(v float64) bool { return v == 0 })
		}
		served(t, "priced on gw-a, the new store's budget unspent", chat(t, a, "priced"))
		allCounted(t, recoverLimit)
	})

	t.Run("SIGTERM flushes the last records before exit", func(t *testing.T) {
		served(t, "chat on gw-b", chat(t, b, "chat"))
		b.stop(t) // well within the 5 s seal interval
		b.logs.wait(t, "the flush", msg("usage flushed", "kaiak.trigger", "drain"))
		// Counted before gw-b exited (the flush was acknowledged); pushed to the
		// observer within the second.
		allCounted(t, waitLimit)
	})

	a.stop(t)
	sample.stop(t)
}

// crossHalfConfig is the e2e config with what this test observes: a second backend
// that refuses everything (the "priced-probe" model), the USD budget of the team
// research (the parent of the key's group eval), counting only the priced models
// ("chat" is unpriced here, so the budget's total is the priced answers alone), a
// global hourly token limit (its total shows on the sample's totals), a per-minute
// limit of 1000 on eval beside metered's 2, and a 1 s outage grace; the
// working backend again as a llama-server, serving the "agent" model over Messages and
// Responses; for the reliability status, a backend refusing connections (the
// "down" model; a circuit opens on its first failure) and one taking one request at
// a time (the "held" model).
func crossHalfConfig(backendURL, refuserURL, downURL, cappedURL, evalHash, annHash string) map[string]any {
	cfg := testConfig(backendURL, evalHash, annHash, "")
	backends := cfg["backends"].(map[string]any)
	backends["refuser"] = map[string]any{"type": "openai-compatible", "base_url": refuserURL + "/v1"}
	backends["down"] = map[string]any{"type": "openai-compatible", "base_url": downURL + "/v1"}
	backends["capped"] = map[string]any{"type": "openai-compatible", "base_url": cappedURL + "/v1", "max_in_flight": 1}
	// The working backend again, as a llama-server: its type serves Messages and
	// Responses (and their token counting), which the openai-compatible type does not.
	backends["ls"] = map[string]any{"type": "llama-server", "base_url": backendURL + "/v1"}
	models := cfg["models"].(map[string]any)
	probe := maps.Clone(models["priced"].(map[string]any))
	probe["deployments"] = []any{map[string]any{"backend": "refuser", "model": backendChatModel}}
	models["priced-probe"] = probe
	for name, backend := range map[string]string{"down": "down", "held": "capped", "agent": "ls"} {
		m := maps.Clone(models["rpm"].(map[string]any))
		m["deployments"] = []any{map[string]any{"backend": backend, "model": backendChatModel}}
		models[name] = m
	}
	global := cfg["global"].(map[string]any)
	global["control_outage_grace_ms"] = 1000
	global["circuit"] = map[string]any{"failure_threshold": 1}
	global["limits"] = []any{map[string]any{"type": "tokens_per_hour", "value": 1_000_000_000}}
	delete(models["chat"].(map[string]any), "prices")
	groups := cfg["groups"].(map[string]any)
	groups["research"].(map[string]any)["limits"] = []any{
		map[string]any{"type": "usd_per_month", "value": 0.0001},
	}
	groups["eval"].(map[string]any)["limits"] = []any{
		map[string]any{"type": "requests_per_minute", "value": 1000},
	}
	return cfg
}

// writeConfigFile replaces the config file in one rename, as an editor saving it
// does: the sample never reads half a file.
func writeConfigFile(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	tmp := path + ".tmp"
	writeJSON(t, tmp, cfg)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// projectedDir is a directory laid out as Kubernetes projects a ConfigMap volume: the
// file is a link to ..data/<name>, ..data a link to a timestamped directory holding
// the content, and an update swaps ..data to a new directory in one rename.
type projectedDir struct {
	dir, name, file string
	current         string // the directory ..data points to; "" before the first project
	n               int
}

func newProjectedDir(t *testing.T, dir, name string) *projectedDir {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &projectedDir{dir: dir, name: name, file: filepath.Join(dir, name)}
}

// project publishes cfg as the volume's content, the way the kubelet's atomic writer
// does: a new ..<stamp> directory, a ..data_tmp link to it renamed over ..data, the
// old directory removed. The first call also creates ..data and the file's link.
func (p *projectedDir) project(t *testing.T, cfg map[string]any) {
	t.Helper()
	p.n++
	stamp := fmt.Sprintf("..2026_09_25_00_00_%02d.%d", p.n, p.n)
	if err := os.Mkdir(filepath.Join(p.dir, stamp), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(p.dir, stamp, p.name), cfg)
	if p.current == "" {
		if err := os.Symlink(stamp, filepath.Join(p.dir, "..data")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", p.name), p.file); err != nil {
			t.Fatal(err)
		}
		p.current = stamp
		return
	}
	tmp := filepath.Join(p.dir, "..data_tmp")
	if err := os.Symlink(stamp, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(p.dir, p.current)); err != nil {
		t.Fatal(err)
	}
	p.current = stamp
}

// requireSample returns the node binary, failing — never skipping — when the sample
// cannot run: this test is only built when asked for.
func requireSample(t *testing.T, root string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the cross-half e2e runs the sample control plane: Node.js must be on PATH (docs/TECH-STACK.md)")
	}
	if _, err := os.Stat(filepath.Join(root, "control", "node_modules", "kaiak-control")); err != nil {
		t.Fatal("the cross-half e2e runs the sample control plane: install its dependencies first (npm ci in control/, " +
			"or run scripts/check-all.sh, which does)")
	}
	return node
}

// sampleProcess is one running sample control plane.
type sampleProcess struct {
	cmd    *exec.Cmd
	logs   *logLines
	url    string
	exited chan struct{}
	err    error
	totals *totalsWatch
	// replicas are the protocol replicas' base URLs: further cores over the sample's
	// one store (KAIAK_SAMPLE_PROTOCOL_PORTS).
	replicas []string
}

// startSample runs the sample on a free loopback port and returns once it logs its
// address. It is killed at test cleanup if still running.
func startSample(t *testing.T, node, root, configFile, token string) *sampleProcess {
	t.Helper()
	return startSampleReplicas(t, node, root, configFile, token, 0)
}

// startSampleReplicas runs the sample with replicas protocol replicas, each on a free
// loopback port, and returns once every listener logged its address.
func startSampleReplicas(t *testing.T, node, root, configFile, token string, replicas int) *sampleProcess {
	t.Helper()
	cmd := exec.Command(node, "src/main.ts")
	cmd.Dir = filepath.Join(root, "control", "sample")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "KAIAK_") && !strings.HasPrefix(kv, "INIT_CWD=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "KAIAK_SAMPLE_CONFIG="+configFile, "KAIAK_CONTROL_TOKEN="+token,
		"KAIAK_SAMPLE_LISTEN=127.0.0.1:0", "KAIAK_LOG_FORMAT=json")
	if replicas > 0 {
		cmd.Env = append(cmd.Env, "KAIAK_SAMPLE_PROTOCOL_PORTS="+strings.Repeat("0,", replicas-1)+"0")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = w, w // the log on stdout; a startup failure on stderr
	s := &sampleProcess{cmd: cmd, logs: newLogLines(), exited: make(chan struct{})}
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() {
		s.logs.read(r)
		r.Close()
		close(readDone)
	}()
	go func() {
		s.err = cmd.Wait()
		<-readDone
		close(s.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-s.exited:
		default:
			_ = cmd.Process.Kill()
			<-s.exited
		}
		if t.Failed() {
			t.Logf("sample log:\n%s", s.logs.text())
		}
	})
	s.url = fmt.Sprint(s.logs.wait(t, "the sample's address", func(entry map[string]any) bool {
		m, _ := entry["msg"].(string)
		return strings.HasPrefix(m, "sample control plane listening on ") && entry["url"] != nil
	})["url"])
	for i := 1; i <= replicas; i++ {
		prefix := fmt.Sprintf("sample protocol replica %d listening on ", i)
		s.replicas = append(s.replicas, fmt.Sprint(s.logs.wait(t, "a replica's address", func(entry map[string]any) bool {
			m, _ := entry["msg"].(string)
			return strings.HasPrefix(m, prefix) && entry["url"] != nil
		})["url"]))
	}
	s.totals = watchTotals(t, s.url, token)
	return s
}

// stop sends SIGTERM and requires a clean exit.
func (s *sampleProcess) stop(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.exited:
	case <-time.After(waitLimit):
		t.Fatalf("the sample did not exit within %s", waitLimit)
	}
	if s.err != nil {
		t.Fatalf("the sample exited with %v", s.err)
	}
}

// totalsWatch follows the sample's totals the way a gateway does: a config stream
// under an instance name of its own. A stream alone does not join the live set (only
// status reports do), so the observer changes no share.
type totalsWatch struct {
	mu      sync.Mutex
	latest  *control.Totals
	changed chan struct{} // closed and replaced on every totals event and at the end
	ended   bool
	err     error // why the stream ended
}

// observerInstance names the test's stream to the control plane.
const observerInstance = "e2e-observer"

// watchTotals opens GET /v1/stream?since=0 on the control plane at baseURL, in the
// config epoch its snapshot names: every config is replayed, then totals follow on
// every change. The stream is closed at test cleanup, and ends by itself when the
// control plane stops.
func watchTotals(t *testing.T, baseURL, token string) *totalsWatch {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Kaiak-Protocol", strconv.Itoa(control.ProtocolVersion))
		req.Header.Set("Kaiak-Instance", observerInstance)
		return http.DefaultClient.Do(req)
	}
	snapshot, err := get("/v1/config")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var current struct {
		ConfigEpoch string `json:"config_epoch"`
	}
	err = json.NewDecoder(snapshot.Body).Decode(&current)
	snapshot.Body.Close()
	if err != nil || snapshot.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("config snapshot: %d %v", snapshot.StatusCode, err)
	}
	resp, err := get("/v1/stream?since=0&config_epoch=" + current.ConfigEpoch)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("totals stream: %d", resp.StatusCode)
	}
	w := &totalsWatch{changed: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		w.read(sse.NewReader(resp.Body, 1<<20))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return w
}

func (w *totalsWatch) read(events *sse.Reader) {
	var err error
	for {
		var block sse.Block
		if block, err = events.Next(); err != nil {
			break
		}
		if block.Event != "totals" {
			continue // config replays and pushes; heartbeats
		}
		var totals control.Totals
		if totals, err = control.DecodeTotals(block.Data); err != nil {
			break
		}
		w.mu.Lock()
		w.latest = &totals
		close(w.changed)
		w.changed = make(chan struct{})
		w.mu.Unlock()
	}
	w.mu.Lock()
	w.ended, w.err = true, err
	close(w.changed)
	w.mu.Unlock()
}

// wait returns the latest totals once match accepts them, waiting for further totals
// events up to limit.
func (w *totalsWatch) wait(t *testing.T, what string, limit time.Duration, match func(control.Totals) bool) control.Totals {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		latest, changed, ended, err := w.latest, w.changed, w.ended, w.err
		w.mu.Unlock()
		switch {
		case latest != nil && match(*latest):
			return *latest
		case ended:
			t.Fatalf("totals stream ended (%v) before %s", err, what)
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("totals: %s not seen within %s; latest %+v", what, limit, latest)
		}
	}
}

// answerUsage is the part of an answer's usage a token limit reads.
type answerUsage struct {
	Total   int64 `json:"total_tokens"`
	Details struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// limitTokens is what a token limit counts of the answer: every token but the input
// read from the cache (GATEWAY.md, Limits → Settle).
func (u answerUsage) limitTokens() int64 { return u.Total - u.Details.Cached }

// servedTokens are the tokens of the answers served, each with when it was received.
// The sample's tokens_per_hour total covers one UTC hour, so a run crossing the top
// of the hour sees it start again from the answers of the new hour.
type servedTokens struct {
	answers []servedAnswer
}

type servedAnswer struct {
	at     time.Time
	tokens int64
}

// hourEdge is how long before an answer's arrival its usage record may be stamped:
// an answer received this close after the top of an hour may count in either hour.
const hourEdge = time.Second

func (s *servedTokens) add(tokens int64) {
	s.answers = append(s.answers, servedAnswer{at: time.Now(), tokens: tokens})
}

func (s *servedTokens) total() int64 {
	var n int64
	for _, a := range s.answers {
		n += a.tokens
	}
	return n
}

// counted reports whether totals' global hourly token total is the answers of its
// window: exactly, but an answer at the window's edges may count in either hour.
func (s *servedTokens) counted(totals control.Totals) bool {
	for _, w := range totals.Windows {
		if w.Group != "" || w.Type != config.LimitTokensPerHour {
			continue
		}
		start, end := w.WindowStart, w.WindowStart.Add(time.Hour)
		var sure, edge int64
		for _, a := range s.answers {
			switch {
			case a.at.Before(start) || !a.at.Before(end.Add(hourEdge)):
			case a.at.Before(start.Add(hourEdge)) || !a.at.Before(end):
				edge += a.tokens
			default:
				sure += a.tokens
			}
		}
		return w.Used >= sure && w.Used <= sure+edge
	}
	return len(s.answers) == 0
}

// used is the window's used amount of group's limit (global: "") of type typ; 0 when
// the totals list no such window.
func used(totals control.Totals, group string, typ config.LimitType) int64 {
	for _, w := range totals.Windows {
		if w.Group == group && w.Type == typ {
			return w.Used
		}
	}
	return 0
}

// poll calls check until it returns true, up to limit: state inside another process
// with no event to wait on.
func poll(limit time.Duration, check func() bool) bool {
	deadline := time.Now().Add(limit)
	for !check() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(metricPoll)
	}
	return true
}

func pollUntil(t *testing.T, what string, limit time.Duration, check func() bool) {
	t.Helper()
	if !poll(limit, check) {
		t.Fatalf("%s: not seen within %s", what, limit)
	}
}

// waitCount waits, up to limit, for the n-th line matching match.
func (l *logLines) waitCount(t *testing.T, what string, n int, limit time.Duration, match func(map[string]any) bool) {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	for {
		l.mu.Lock()
		seen := 0
		for _, entry := range l.lines {
			if match(entry) {
				seen++
			}
		}
		changed, eof := l.changed, l.eof
		l.mu.Unlock()
		switch {
		case seen >= n:
			return
		case eof:
			t.Fatalf("process exited before logging %s", what)
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("%s not logged within %s (%d of %d)", what, limit, seen, n)
		}
	}
}
