//go:build crosshalf

package e2e

// Request capture across both halves (docs/specs/GATEWAY.md, Request capture;
// docs/specs/CONTROL-PROTOCOL.md, Config → The group tree: capture): the sample
// control plane serves a config whose group captures, and a gateway it configures
// captures that group's requests and no other's; a publish that turns the flag off
// stops the captures — the next request makes no span.

import (
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"kaiak/internal/fakebackend"
	"kaiak/internal/telemetry/fakeotlp"
)

func TestAcrossHalvesCapture(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	node := requireSample(t, root)
	backend := fakebackend.New()
	defer backend.Close()

	const token = "e2e-capture-token"
	configFile := filepath.Join(t.TempDir(), "sample-config.json")
	evalKey, evalHash := newKey()
	annKey, annHash := newKey()
	cfg := testConfig(backend.URL(), evalHash, annHash, "")
	eval := cfg["groups"].(map[string]any)["eval"].(map[string]any)
	eval["capture"] = true
	writeConfigFile(t, configFile, cfg)
	sample := startSample(t, node, root, configFile, token)
	collector := fakeotlp.New(t, nil)
	g := startGatewayEnv(t, append(controlEnv(sample.url, token), tracesTo(collector)))
	g.logs.wait(t, "the boot from the sample", msg("config applied", "kaiak.trigger", "control"))

	chat := func(id, key string) map[string]any {
		t.Helper()
		if r := g.post(t, "/v1/chat/completions", key, id, chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", id, r.StatusCode, r.body)
		}
		return g.settled(t, id)
	}
	captured := chat("cross-captured", evalKey)
	if captured["kaiak.capture"] != "queued" {
		t.Errorf("eval's request line %v, want kaiak.capture queued", captured)
	}
	if line := chat("cross-other-group", annKey); line["kaiak.capture"] != nil {
		t.Errorf("ann's request line %v, want no capture: ann does not capture", line)
	}
	expectSpan(t, waitSpans(t, collector, "cross-captured")["cross-captured"], captured, merged(answeredBy(t, backend.URL()),
		map[string]any{"kaiak.key.root_group": "research", "gen_ai.input.messages": helloInput,
			"gen_ai.output.messages": helloOutput, "gen_ai.response.id": "fake-1", "gen_ai.response.model": "chat",
			"gen_ai.response.finish_reasons": []any{"stop"}}))

	// The app turns eval's capture off: the sample publishes it, the gateway applies it.
	eval["capture"] = false
	writeConfigFile(t, configFile, cfg)
	g.logs.waitCount(t, "the published config", 2, waitLimit, msg("config applied", "kaiak.trigger", "control"))
	if line := chat("cross-not-captured", evalKey); line["kaiak.capture"] != nil {
		t.Errorf("eval's request line after the publish %v, want no capture", line)
	}
	g.stop(t) // the trace export's final flush: every span made is at the collector
	if got := slices.Sorted(maps.Keys(requestSpans(t, collector))); !slices.Equal(got, []string{"cross-captured"}) {
		t.Errorf("spans for %q, want cross-captured's alone", got)
	}
}
