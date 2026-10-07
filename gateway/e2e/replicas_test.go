//go:build crosshalf

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/fakebackend"
)

// TestAcrossHalvesReplicas runs two gateways against two control-plane cores over one
// store (the sample with a protocol replica), each gateway behind a proxy of its own
// standing in for a load balancer: usage from both counts once, both cores serve the
// same totals, a config published through one core reaches the gateway of the other,
// and a gateway whose core goes away carries on with the other core while the first
// gateway is undisturbed (CONTROL-PROTOCOL.md, Control-plane
// processes).
func TestAcrossHalvesReplicas(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	node := requireSample(t, root)

	backend := fakebackend.New()
	defer backend.Close()

	const token = "e2e-replicas-token"
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	evalKey, evalHash := newKey()
	_, annHash := newKey()
	url := backend.URL()
	cfg := crossHalfConfig(url, url, url, url, evalHash, annHash)
	writeConfigFile(t, configFile, cfg)

	sample := startSampleReplicas(t, node, root, configFile, token, 1)
	replicaTotals := watchTotals(t, sample.replicas[0], token)
	proxyA, proxyB := newControlProxy(t), newControlProxy(t)
	proxyA.setUpstream(t, sample.url)
	proxyB.setUpstream(t, sample.replicas[0])

	gatewayEnv := func(proxy *controlProxy, instance string) []string {
		return append(controlEnv(proxy.URL(), token, filepath.Join(dir, instance)), "KAIAK_INSTANCE_ID="+instance)
	}
	// Only gw-a has the key of the backend the last config adds, so gw-b rejects it.
	const keyedEnv = "E2E_REPLICAS_KEYED_KEY"
	a := startGatewayEnv(t, append(gatewayEnv(proxyA, "gw-a"), keyedEnv+"=e2e-keyed-key"))
	b := startGatewayEnv(t, gatewayEnv(proxyB, "gw-b"))
	for _, g := range []*gateway{a, b} {
		g.logs.wait(t, "the boot from its core", msg("config applied", "kaiak.trigger", "control"))
	}

	var tokens servedTokens
	serve := func(t *testing.T, g *gateway, model string) {
		t.Helper()
		r := g.post(t, "/v1/chat/completions", evalKey, "", chatBody(model, false, nil))
		if r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s, want 200", model, r.StatusCode, r.body)
		}
		var body struct {
			Usage answerUsage `json:"usage"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil || body.Usage.Total <= 0 {
			t.Fatalf("%s: no usage in %s (%v)", model, r.body, err)
		}
		tokens.add(body.Usage.limitTokens())
	}
	bothCount := func(t *testing.T) {
		t.Helper()
		for name, w := range map[string]*totalsWatch{"the app's core": sample.totals, "the replica": replicaTotals} {
			w.wait(t, fmt.Sprintf("%s: the hourly token total at %d", name, tokens.total()), waitLimit, tokens.counted)
		}
	}
	never := func(t *testing.T, g *gateway, what string, match func(map[string]any) bool) {
		t.Helper()
		g.logs.mu.Lock()
		defer g.logs.mu.Unlock()
		for _, entry := range g.logs.lines {
			if match(entry) {
				t.Fatalf("%s logged %v", what, entry)
			}
		}
	}

	t.Run("both cores count the gateways of both in one live set", func(t *testing.T) {
		for name, w := range map[string]*totalsWatch{"the app's core": sample.totals, "the replica": replicaTotals} {
			w.wait(t, name+": two live gateways", waitLimit, func(tot control.Totals) bool { return tot.LiveGateways == 2 })
		}
	})

	t.Run("usage through either core counts once, in totals both cores serve", func(t *testing.T) {
		for range 3 {
			serve(t, a, "chat")
			serve(t, b, "chat")
		}
		bothCount(t)
	})

	t.Run("a config published through the app's core reaches the replica's gateway", func(t *testing.T) {
		cfg["models"].(map[string]any)["chat-2"] = cfg["models"].(map[string]any)["rpm"]
		writeConfigFile(t, configFile, cfg)
		for _, g := range []*gateway{a, b} {
			g.logs.waitCount(t, "the published edit", 2, waitLimit, msg("config applied", "kaiak.trigger", "control"))
		}
		serve(t, b, "chat-2")
		bothCount(t)
	})

	t.Run("a gateway whose core goes away carries on with the other, the first undisturbed", func(t *testing.T) {
		// The load balancer in front of the replica loses it and sends gw-b to the app's
		// core: the open stream breaks, the gateway reconnects there and takes the
		// current config (the one it runs: skipped) and totals.
		controlApplied := msg("config applied", "kaiak.trigger", "control")
		connects, applied := b.logs.count(msg("config stream connected")), b.logs.count(controlApplied)
		proxyB.setUpstream(t, sample.url)
		proxyB.server.CloseClientConnections()
		b.logs.wait(t, "the broken stream", func(entry map[string]any) bool {
			return msg("config stream ended by the control plane")(entry) || msg("config stream failed")(entry)
		})
		b.logs.waitCount(t, "the reconnect", connects+1, waitLimit, msg("config stream connected"))
		serve(t, a, "chat")
		serve(t, b, "chat")
		bothCount(t)
		if n := b.logs.count(controlApplied); n != applied {
			t.Errorf("gw-b applied %d control configs after the reconnect, want %d: the config it runs is skipped", n, applied)
		}

		cfg["models"].(map[string]any)["chat-3"] = cfg["models"].(map[string]any)["rpm"]
		writeConfigFile(t, configFile, cfg)
		for _, g := range []*gateway{a, b} {
			g.logs.waitCount(t, "the next edit", 3, waitLimit, msg("config applied", "kaiak.trigger", "control"))
		}
		serve(t, b, "chat-3")
		bothCount(t)

		never(t, a, "a broken stream on the undisturbed gateway", msg("config stream failed"))
	})

	t.Run("a gateway that rejected a config dropping a budget still enforces its spend", func(t *testing.T) {
		// The next config drops research's budget and adds a backend whose key only
		// gw-a has: gw-a applies it, gw-b rejects it and runs on with the budget. The
		// control plane counts research's spend whatever its config, and the totals
		// carry it, so gw-b refuses the budget's models once gw-a spent it.
		cfg["groups"].(map[string]any)["research"].(map[string]any)["limits"] = []any{}
		cfg["backends"].(map[string]any)["keyed"] = map[string]any{
			"type": "openai-compatible", "base_url": url + "/v1", "api_key_env": keyedEnv}
		writeConfigFile(t, configFile, cfg)
		a.logs.waitCount(t, "the budget dropped", 4, waitLimit, msg("config applied", "kaiak.trigger", "control"))
		b.logs.wait(t, "the config rejected", msg("config rejected", "kaiak.trigger", "control"))
		b.waitMetric(t, "the rejection", "kaiak_control_config_rejected", func(v float64) bool { return v == 1 })

		if r := a.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("priced on gw-a, its budget dropped: %d %s, want 200", r.StatusCode, r.body)
		}
		replicaTotals.wait(t, "research's spend counted without its budget", waitLimit, func(tot control.Totals) bool {
			return used(tot, "research", config.LimitUSDPerMonth) >= 150_000
		})
		// In this test every backend answers, so an answer before gw-b has the totals
		// spends a little more; the refusal must come.
		pollUntil(t, "gw-b refusing the budget it still runs", waitLimit, func() bool {
			r := b.post(t, "/v1/chat/completions", evalKey, "", chatBody("priced", false, nil))
			switch {
			case r.StatusCode == http.StatusTooManyRequests && r.errorCode(t) == "budget_exceeded":
				return true
			case r.StatusCode != http.StatusOK:
				t.Fatalf("priced on gw-b: %d %s", r.StatusCode, r.body)
			}
			return false
		})
	})
}
