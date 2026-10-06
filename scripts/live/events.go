package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicVersion is the anthropic-version header the kit sends, as Anthropic's SDKs
// do.
const anthropicVersion = "2023-06-01"

// typedEvent is one server-sent event of a Messages or Responses stream: its data,
// whose type member names it (both formats repeat the event name there).
type typedEvent struct {
	Type string
	Data map[string]any
	Raw  string
}

// openTypedStream sends req and checks it answered 200 with an event stream; on any
// other answer it returns the failure to report, with the gateway's view of it.
func (r *run) openTypedStream(req *http.Request, id string) (*http.Response, string) {
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Sprintf("status %d, content type %q: %s%s", resp.StatusCode, resp.Header.Get("Content-Type"),
			clip(string(body)), r.upstreamHint(id))
	}
	return resp, ""
}

// readTypedEvents reads a Messages or Responses stream to its end. A data field that
// is not a JSON object is an error, and so is OpenAI's [DONE], which neither format
// sends.
func readTypedEvents(body io.Reader) ([]typedEvent, error) {
	var events []typedEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimPrefix(data, " ")
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return events, fmt.Errorf("event data is not a JSON object: %s", clip(data))
		}
		typ, _ := ev["type"].(string)
		events = append(events, typedEvent{Type: typ, Data: ev, Raw: data})
	}
	if err := sc.Err(); err != nil {
		return events, fmt.Errorf("stream broke off after %d events: %w", len(events), err)
	}
	return events, nil
}

// field reads a nested member of a decoded JSON object, nil when any step is missing.
func field(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

// str and num read a nested string or number ("" and 0 when absent).
func str(v any, path ...string) string {
	s, _ := field(v, path...).(string)
	return s
}

func num(v any, path ...string) float64 {
	n, _ := field(v, path...).(float64)
	return n
}

// apiError is the part of an error answer the checks read, in either shape: OpenAI's
// {"error": {"message", "type", "param", "code"}} or Anthropic's {"type": "error",
// "error": {"type", "message", "code"}} — kaiak adds its code to Anthropic's.
type apiError struct {
	Type  string
	Error struct {
		Type  string
		Code  any
		Param any
	}
}

// anthropicError checks an answer is status in Anthropic's error shape with errType
// and kaiak's code; "" when it is, else what is wrong.
func anthropicError(resp *response, status int, errType, code string) string {
	var e apiError
	_ = json.Unmarshal(resp.body, &e)
	switch {
	case resp.status != status:
		return fmt.Sprintf("status %d, want %d: %s", resp.status, status, clip(string(resp.body)))
	case e.Type != "error" || e.Error.Type != errType || e.Error.Code != code:
		return fmt.Sprintf("body %s, want Anthropic's shape with type %s and code %s", clip(string(resp.body)), errType, code)
	}
	return ""
}

// openAIError checks an answer is status in OpenAI's error shape with kaiak's code
// (and param, when one is given); "" when it is, else what is wrong.
func openAIError(resp *response, status int, code string, param ...string) string {
	var e apiError
	_ = json.Unmarshal(resp.body, &e)
	switch {
	case resp.status != status:
		return fmt.Sprintf("status %d, want %d: %s", resp.status, status, clip(string(resp.body)))
	case e.Error.Code != code:
		return fmt.Sprintf("body %s, want code %s", clip(string(resp.body)), code)
	case len(param) > 0 && e.Error.Param != param[0]:
		return fmt.Sprintf("param %v, want %s", e.Error.Param, param[0])
	}
	return ""
}

// notSent reports whether the request's log line shows it never reached a backend: a
// refusal before routing.
func (r *run) notSent(id string) (bool, error) {
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
	if err != nil {
		return false, err
	}
	_, routed := line["kaiak.backend.id"]
	return !routed, nil
}

// noUsageRecord checks a token-counting request's log line: answered 200 and settled
// no usage (nothing is billed); "" when so, else what is wrong.
func (r *run) noUsageRecord(id string) string {
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
	switch {
	case err != nil:
		return fmt.Sprintf("no log line for request %s: %v", id, err)
	case line["http.response.status_code"] != 200.0:
		return fmt.Sprintf("logged status %v", line["http.response.status_code"])
	case line["kaiak.usage.estimated"] != nil:
		return fmt.Sprintf("a usage record was settled (in %v, out %v)", line["gen_ai.usage.input_tokens"], line["gen_ai.usage.output_tokens"])
	}
	return ""
}

// withParams adds params to a request body and returns it.
func withParams(body, params map[string]any) map[string]any {
	for k, v := range params {
		body[k] = v
	}
	return body
}
