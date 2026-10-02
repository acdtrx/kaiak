package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"kaiak/internal/fakebackend"
)

// pathModules are the backend types with their server's answer to a path it does not
// have (docs/specs/GATEWAY.md, Providers: wrong path to a host); suffix is what the
// type's base_url adds to the fake backend's address.
var pathModules = []struct {
	typ, suffix, answer string
}{
	{"openai", "/v1", `{"error":{"message":"Invalid URL (POST /chat/completions)","type":"invalid_request_error","param":null,"code":null}}`},
	{"azure-openai", "", `{"error":{"code":"404","message":"Resource not found"}}`},
	{"vllm", "/v1", `{"detail":"Not Found"}`},
	{"llama-server", "/v1", `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`},
	{"openai-compatible", "/v1", "404 page not found\n"},
}

// localEntry is the test config's backend "local" up to its base_url's /v1.
var localEntry = regexp.MustCompile(`"local": \{ "type": "openai-compatible", "base_url": "([^"]*)/v1"`)

// localAs makes backend "local" one of type typ, its base_url ending in suffix (it
// keeps its api_key_env, which openai and azure-openai require).
func localAs(typ, suffix string) func(string) string {
	return func(doc string) string {
		return localEntry.ReplaceAllString(doc, `"local": { "type": "`+typ+`", "base_url": "${1}`+suffix+`"`)
	}
}

// Per module: the server's unknown-path answer is the deployment's failure — retried
// on another deployment, a circuit failure, no usage record of its own, counted as
// path_missing — and with no deployment left the client gets 502
// upstream_path_missing without the backend's text, and its record no usage.
func TestUnknownPathIsTheDeploymentsFailure(t *testing.T) {
	for _, m := range pathModules {
		t.Run(m.typ+"/retried", func(t *testing.T) {
			circuit := withGlobal(`"circuit": { "failure_threshold": 1, "probe_interval_ms": 3600000 }`)
			g, other := newRetryGateway(t, "local", "local-b", func(doc string) string {
				return circuit(localAs(m.typ, m.suffix)(doc))
			})
			if got := g.holder.Current().Backends["local"].Type; string(got) != m.typ {
				t.Fatalf("backend local is %s, want %s", got, m.typ)
			}
			g.backend.QueueReplies(fakebackend.Reply{Status: http.StatusNotFound, Body: m.answer})
			w := post(t, g, "r", `{"model":"retry"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 from the second deployment: %s", w.Code, w.Body.String())
			}
			if len(other.Requests()) != 1 {
				t.Errorf("second deployment got %d requests, want 1", len(other.Requests()))
			}
			line := logLine(t, g, "r")
			if want := `"tried":"local/first:upstream_path_missing,local-b/second:200"`; !strings.Contains(line, want) {
				t.Errorf("log line misses %s:\n%s", want, line)
			}
			records := recordsOf(g, "r")
			if len(records) != 1 || records[0].Deployment.Backend != "local-b" {
				t.Errorf("records %+v, want the second deployment's alone", records)
			}
			if !circuitOpen(g, "local", "first") {
				t.Error("circuit of local/first closed, want open: the outcome is a failure")
			}
			expectMetricLines(t, scrape(g),
				`kaiak_retries_total{model="retry",backend="local",reason="path_missing"} 1`,
				`kaiak_upstream_attempts_total{backend="local",deployment_model="first",outcome="path_missing"} 1`)
		})
		t.Run(m.typ+"/no deployment left", func(t *testing.T) {
			g := newTestGateway(t)
			s := testSnapshotWith(t, g.backend.URL(), localAs(m.typ, m.suffix))
			g.holder.Swap(s)
			g.router.Configure(s)
			g.backend.SetReply(fakebackend.Reply{Status: http.StatusNotFound, Body: m.answer})
			w := post(t, g, "r", `{"model":"open"}`)
			expectError(t, w, http.StatusBadGateway, "upstream_path_missing")
			for _, text := range []string{"Invalid URL", "Not Found", "not found"} {
				if strings.Contains(w.Body.String(), text) {
					t.Errorf("the backend's answer reached the client: %s", w.Body.String())
				}
			}
			if n := len(g.backend.Requests()); n != 1 {
				t.Errorf("backend got %d requests, want 1", n)
			}
			// The request's record carries no usage: nothing was processed.
			records := recordsOf(g, "r")
			if len(records) != 1 {
				t.Fatalf("%d records, want the request's one", len(records))
			}
			expectUnits(t, records[0], units(0, 0, 0, 0, 0), false, true)
			expectMetricLines(t, scrape(g), `kaiak_errors_total{class="upstream_error"} 1`)
		})
	}
}

// A wrong path belongs to the backend, as a refused credential does: every
// deployment of the model on that backend shares its base_url, so the retry skips
// them all and goes to another backend — never to a sibling on the same one.
func TestUnknownPathRefusesTheBackendForTheRequest(t *testing.T) {
	g, other := newRetryGateway(t, "local", "local-b", func(doc string) string {
		return strings.Replace(doc, `{ "backend": "local", "model": "first" }, `,
			`{ "backend": "local", "model": "first" }, { "backend": "local", "model": "sibling" }, `, 1)
	})
	if n := len(g.holder.Current().Models["retry"].Deployments); n != 3 {
		t.Fatalf("model retry has %d deployments, want 3", n)
	}
	g.backend.SetReply(fakebackend.Reply{Status: http.StatusNotFound, Body: "404 page not found\n"})
	w := post(t, g, "r", `{"model":"retry"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 from local-b: %s", w.Code, w.Body.String())
	}
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend local got %d requests, want 1: its sibling deployment was tried", n)
	}
	if n := len(other.Requests()); n != 1 {
		t.Errorf("local-b got %d requests, want 1", n)
	}
	if want := `"attempts":2,"tried":"local/first:upstream_path_missing,local-b/second:200"`; !strings.Contains(logLine(t, g, "r"), want) {
		t.Errorf("log line misses %s:\n%s", want, logLine(t, g, "r"))
	}
}
