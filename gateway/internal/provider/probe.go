package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/netfail"
)

// The models list a module's probe reads, and the base_url hint its 404 carries.

// probeReadTimeout bounds a probe's wait for the answer once connected; the
// backend's connect timeout bounds the connection before it. The models list is
// cheap: a backend that takes longer is not ready for traffic.
const probeReadTimeout = 5 * time.Second

// maxProbeBody is the most of a probe's answer read, so the connection returns to
// the pool; a longer answer closes it (and is not a models list the probe reads).
const maxProbeBody = 1 << 20

// fetchModelsList asks backend b for its models list at url, with header (the
// module's credential), bounded by b's connect timeout plus probeReadTimeout, and
// returns the answer's body when the status is 2xx. A 404 is a *PathMissingError
// carrying pathHint, the module's word on what base_url should hold. The error may
// name the backend's address, never the credential nor text the backend sent: a
// transport failure is named by its class (netfail).
func fetchModelsList(ctx context.Context, b *config.Backend, client *http.Client, url string, header http.Header,
	pathHint string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, b.ConnectTimeout+probeReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %s: build probe: %w", b.ID, err)
	}
	req.Header = header.Clone()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kaiak")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backend %s: %s", b.ID, netfail.Class(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, fmt.Errorf("backend %s: read models list: %s", b.ID, netfail.Class(err))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &PathMissingError{Backend: b.ID, URL: url, Hint: pathHint}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("backend %s: models list answered %d", b.ID, resp.StatusCode)
	}
	return body, nil
}

// PathMissingError is a probe's error when the backend answered 404 for its models
// list: the server is up, and the backend's base_url is most likely wrong.
type PathMissingError struct {
	Backend string
	// URL is the models list's address; Hint says what the module expects base_url
	// to hold.
	URL  string
	Hint string
}

func (e *PathMissingError) Error() string {
	return fmt.Sprintf("backend %s: models list answered 404 at %s", e.Backend, e.URL)
}

// BaseURLHint says what the backend's base_url should hold.
func (e *PathMissingError) BaseURLHint() string { return e.Hint }

// versionPathHint is the base_url hint of the modules whose base_url is what an
// OpenAI client would use (docs/specs/GATEWAY.md, Base URLs).
const versionPathHint = "base_url should end in the API version path, e.g. /v1"

// listedModels reads an OpenAI models list and reports, through serves, whether a
// backend-side model name is one of its data[*].id.
func listedModels(b *config.Backend, body []byte) (serves func(model string) bool, err error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		return nil, fmt.Errorf("backend %s: the answer is not a models list", b.ID)
	}
	ids := make(map[string]bool, len(list.Data))
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	return func(model string) bool { return ids[model] }, nil
}
