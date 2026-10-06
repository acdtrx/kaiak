package control

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kaiak/internal/fakecontrol"
)

// withSeed gives a client the seed config seed.
func withSeed(seed []byte) func(*Options) {
	return func(o *Options) { o.SeedConfig, o.SeedFile = seed, "seed.json" }
}

// E2: a boot with the control plane out of reach and no last-known-good copy serves
// the seed config. It is never saved as last-known-good, the status says ready with
// no control-plane config hash (N-P11), and the first config from the control plane
// replaces it.
func TestSeedConfigServesABootWithTheControlPlaneDown(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.cp.SetDown(true)
	c := h.client(withSeed(configB(t)))
	if err := h.boot(c); err != nil {
		t.Fatalf("Boot with a seed config: %v", err)
	}
	h.wantLoad(load{TriggerSeed, true})
	if h.holder.Current().Models["large"] == nil {
		t.Fatal("the seed config is not in the holder")
	}
	if _, ok := c.AppliedConfigHash(); ok {
		t.Error("the seed config counts as a control-plane config")
	}
	if s := c.currentStatus(); s.State != StateReady || s.AppliedConfigHash != nil {
		t.Errorf("status after a seed boot: state %s, config %v; want ready with no config hash", s.State,
			s.AppliedConfigHash)
	}
	if got := h.savedHash(); got != "" {
		t.Errorf("last-known-good hash %s after a seed boot, want none", got)
	}
	if !strings.Contains(h.logs.String(), `msg="config applied" kaiak.trigger=seed file.path=seed.json`) {
		t.Errorf("seed load not logged:\n%s", h.logs.String())
	}

	stop := h.run(c)
	h.cp.SetDown(false)
	h.wantLoad(load{TriggerControl, true})
	if h.holder.Current().Models["llama"] == nil {
		t.Fatal("the control plane's config did not replace the seed")
	}
	stop() // the last-known-good write follows the apply on the client's goroutine
	if got, want := h.savedHash(), fakecontrol.Hash(configA(t)); got != want {
		t.Errorf("last-known-good hash %s, want the control plane's %s", got, want)
	}
}

// With a data directory the last-known-good copy wins over the seed: it is a config
// the control plane sent.
func TestLastKnownGoodWinsOverTheSeedConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	h.boot(h.client(nil))
	h.wantLoad(load{TriggerControl, true})

	h.cp.SetDown(true)
	h.holder.Swap(nil) // a new process on the same data directory
	if err := h.client(withSeed(configB(t))).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantLoad(load{TriggerLastKnownGood, true})
	h.noLoadPending()
	if h.holder.Current().Models["llama"] == nil {
		t.Fatal("the last-known-good config is not in the holder")
	}
}

// A reachable control plane's config is used, the seed ignored.
func TestSeedConfigIsIgnoredWhenTheControlPlaneAnswers(t *testing.T) {
	h := newHarness(t)
	h.cp.Publish(configA(t))
	if err := h.client(withSeed(configB(t))).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantLoad(load{TriggerControl, true})
	h.noLoadPending()
	if h.holder.Current().Models["llama"] == nil {
		t.Fatal("the control plane's config is not in the holder")
	}
}

// E2: a control plane with nothing published cannot give a config within the boot
// wait: the seed serves.
func TestSeedConfigServesWhenTheControlPlaneHasNoConfig(t *testing.T) {
	h := newHarness(t)
	if err := h.client(withSeed(configB(t))).Boot(context.Background()); err != nil {
		t.Fatalf("Boot with a seed and no config published: %v", err)
	}
	h.wantLoad(load{TriggerSeed, true})
}

// errorControlPlane answers every request with status and code, speaking the
// protocol.
func errorControlPlane(t *testing.T, status int, code string) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Kaiak-Protocol", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// E2: a control plane failing on its side (5xx) is unavailable too: the seed serves.
func TestSeedConfigServesWhenTheControlPlaneFails(t *testing.T) {
	h := newHarness(t)
	u := errorControlPlane(t, http.StatusInternalServerError, "internal-error")
	if err := h.client(func(o *Options) { withSeed(configB(t))(o); o.URL = u }).Boot(context.Background()); err != nil {
		t.Fatalf("Boot with a seed and a failing control plane: %v", err)
	}
	h.wantLoad(load{TriggerSeed, true})
}

// E2: what the operator must fix — a refused token, a config the gateway rejects —
// is never covered up by the seed: Boot fails, naming the cause.
func TestSeedConfigIsNotUsedForAnOperatorError(t *testing.T) {
	t.Run("refused token", func(t *testing.T) {
		h := newHarness(t)
		h.cp.Publish(configA(t))
		err := h.client(func(o *Options) { withSeed(configB(t))(o); o.Token = "wrong" }).Boot(context.Background())
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "no config") {
			t.Fatalf("Boot error %v, want no config naming the 401", err)
		}
		h.noLoadPending()
	})
	t.Run("rejected config", func(t *testing.T) {
		h := newHarness(t)
		h.cp.Publish(configRejected(t))
		err := h.client(withSeed(configB(t))).Boot(context.Background())
		if err == nil || !strings.Contains(err.Error(), "was rejected") || !strings.Contains(err.Error(), "key-group-unknown") {
			t.Fatalf("Boot error %v, want no config naming the rejection and its codes", err)
		}
		h.wantLoad(load{TriggerControl, false})
		h.noLoadPending()
	})
}

// E2: with neither the control plane, a last-known-good copy nor a seed, Boot fails
// with the message the process exits on.
func TestBootFailsWithNoSourceOfConfig(t *testing.T) {
	h := newHarness(t)
	h.cp.SetDown(true)
	err := h.boot(h.client(nil))
	if err == nil || !strings.Contains(err.Error(), "no config: control plane unavailable and no seed") {
		t.Fatalf("Boot error %v", err)
	}
	if h.holder.Loaded() {
		t.Fatal("holder loaded")
	}
}

// A last-known-good file of the previous format (a format-3 config inside, from
// before tokens_cache_write) is discarded and logged: the boot falls back to the seed.
func TestLastKnownGoodOfAnotherFormatIsDiscarded(t *testing.T) {
	h := newHarness(t)
	old := lastKnownGood{ConfigHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Config: []byte(`{"format_version": 3, "models": {"m": {"prices": [{"effective_from": "2026-01-01", ` +
			`"tiers": [{"above_input_tokens": 0, "usd_per_million": {"tokens_in": 1}}]}]}}}`)}
	if err := h.dir.WriteVersioned(LastKnownGoodFile, lastKnownGoodFormat-1, old); err != nil {
		t.Fatal(err)
	}
	h.cp.SetDown(true)
	if err := h.client(withSeed(configB(t))).Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.wantLoad(load{TriggerSeed, true})
	h.noLoadPending()
	if out := h.logs.String(); !strings.Contains(out, "discarded data file with a different format version") ||
		!strings.Contains(out, fmt.Sprintf("kaiak.data_file.found_version=%d", lastKnownGoodFormat-1)) {
		t.Errorf("discard not logged:\n%s", out)
	}
}
