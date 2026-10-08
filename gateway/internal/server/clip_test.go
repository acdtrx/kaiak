package server

import (
	"net/http"
	"strings"
	"testing"

	"kaiak/internal/clip"
)

// Strings the client controls — the path, the method, a refused model name —
// are clipped to clip.Max bytes wherever they are logged or echoed in an error
// message; the key never appears in either.
func TestClientStringsAreClippedInLogsAndErrors(t *testing.T) {
	g := newTestGateway(t)
	huge := strings.Repeat("x", 1<<20)
	bodyModel := strings.Repeat("m", 900) // fits the test body cap
	cases := []struct {
		name   string
		c      call
		status int
		code   string
	}{
		{"1 MiB path", call{method: "GET", path: "/" + huge, key: userKey}, http.StatusNotFound, "unknown_url"},
		{"huge method", call{method: strings.Repeat("M", 4096), path: "/v1/chat/completions", key: userKey},
			http.StatusMethodNotAllowed, "method_not_allowed"},
		{"1 MiB model in the path", call{method: "GET", path: "/v1/models/" + huge, key: userKey},
			http.StatusNotFound, "model_not_found"},
		{"huge model in the body", call{method: "POST", path: "/v1/chat/completions", key: userKey,
			body: `{"model":"` + bodyModel + `","messages":[]}`}, http.StatusNotFound, "model_not_found"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := "clip-" + string(rune('a'+i))
			c.c.header = map[string]string{"X-Request-Id": id}
			w := do(t, g.h, c.c)
			expectError(t, w, c.status, c.code)
			if n := w.Body.Len(); n > 2*clip.Max+200 {
				t.Errorf("error body of %d bytes: a client string was echoed whole", n)
			}
			line := logLine(t, g, id)
			if n := len(line); n > 3*clip.Max+600 {
				t.Errorf("log line of %d bytes: a client string was logged whole", n)
			}
			if !strings.Contains(line, clip.Marker) || !strings.Contains(w.Body.String(), clip.Marker) {
				t.Errorf("no clip marker in the log line or the error:\n%.2000s\n%.2000s", line, w.Body.String())
			}
			if strings.Contains(line, userKey) || strings.Contains(w.Body.String(), userKey) {
				t.Error("the key appears in the log line or the error")
			}
		})
	}
}
