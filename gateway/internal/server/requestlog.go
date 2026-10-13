package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/clip"
	"kaiak/internal/config"
	"kaiak/internal/logattr"
	"kaiak/internal/provider"
)

// logRequest writes the one log line per request, in the log vocabulary
// (docs/specs/GATEWAY.md, Observability: Logs → the request line). It never carries
// the key, a header value or any request or response content; strings the client
// controls (method, path, model) are clipped.
func (a *API) logRequest(rq *request) {
	attrs := []slog.Attr{slog.String("kaiak.request.id", rq.id)}
	attrs = append(attrs, methodAttrs(rq.r.Method)...)
	attrs = append(attrs, slog.String("url.path", clip.String(rq.r.URL.Path)))
	if status, ok := statusSent(rq); ok {
		attrs = append(attrs, slog.Int("http.response.status_code", status))
	}
	attrs = append(attrs, logattr.SecondsMicro("kaiak.request.duration", time.Since(rq.start)))
	if rq.keyID != "" {
		attrs = append(attrs, slog.String("kaiak.key.id", rq.keyID))
	}
	// The key's group: its config ID, as on the usage metrics; never its labels.
	if g := rq.identity.Group; g != nil {
		attrs = append(attrs, slog.String("kaiak.key.group", g.ID))
	}
	if rq.model != "" {
		attrs = append(attrs, slog.String("gen_ai.request.model", clip.String(rq.model)))
	}
	if rq.modelAllowed && rq.endpoint.body {
		attrs = append(attrs, slog.Bool("gen_ai.request.stream", rq.inbound.Stream))
	}
	if rq.endpoint != nil && rq.endpoint.operation != "" {
		attrs = append(attrs, slog.String("gen_ai.operation.name", rq.endpoint.operation))
	}
	if rq.endpoint != nil && rq.endpoint.outputType != "" {
		attrs = append(attrs, slog.String("gen_ai.output.type", rq.endpoint.outputType))
	}
	// The upstream fields are the answering attempt's: an attempt that was retried
	// shows only in kaiak.tried.
	at := rq.answeringAttempt()
	if b := at.deployment.Backend; b != nil {
		attrs = append(attrs, slog.String("kaiak.backend.id", b.ID),
			slog.String("kaiak.backend.type", string(b.Type)),
			slog.String("gen_ai.provider.name", provider.ProviderName(b.Type)),
			slog.String("kaiak.deployment.model", at.deployment.Model),
			slog.Int("kaiak.attempts", len(rq.attempts)))
	}
	if len(rq.attempts) > 1 {
		attrs = append(attrs, slog.String("kaiak.tried", triedAttempts(rq)))
	}
	if rq.retryRefusal != "" {
		attrs = append(attrs, slog.String("kaiak.retry_refused", rq.retryRefusal))
	}
	if rq.queueWait.Queued {
		attrs = append(attrs, logattr.SecondsMicro("kaiak.queue.wait_duration", rq.queueWait.Duration))
	}
	if code := errorCode(rq); code != "" {
		attrs = append(attrs, slog.String("error.type", code))
	}
	if rej := rq.rejection; rej != nil {
		attrs = append(attrs, rej.LogAttrs()...)
	}
	if !rq.firstContent.IsZero() {
		attrs = append(attrs, logattr.SecondsMicro("kaiak.time_to_first_token", rq.ttft))
	}
	if rq.relayEnd != "" {
		attrs = append(attrs, slog.String("kaiak.relay_end", rq.relayEnd))
	}
	if at.err != nil {
		attrs = append(attrs, slog.String("kaiak.upstream.error.message", at.err.Error()))
	}
	if at.errorCode != "" {
		attrs = append(attrs, slog.String("kaiak.upstream.error.code", at.errorCode))
	}
	if at.errorType != "" {
		attrs = append(attrs, slog.String("kaiak.upstream.error.type", at.errorType))
	}
	if rq.authFailure != "" {
		attrs = append(attrs, slog.String("kaiak.auth.failure", string(rq.authFailure)))
	}
	if u := rq.usage; u != nil {
		// Units and cost are the request's (totalUsage); the flags are the answering
		// attempt's.
		units, cost := rq.totalUsage()
		tokens := tokensOf(units)
		attrs = append(attrs,
			slog.Int64("gen_ai.usage.input_tokens", tokens.input),
			slog.Int64("gen_ai.usage.cache_read.input_tokens", tokens.cacheRead),
			slog.Int64("gen_ai.usage.cache_write.input_tokens", tokens.cacheWrite),
			slog.Int64("gen_ai.usage.output_tokens", tokens.output),
			slog.Int64("gen_ai.usage.reasoning.output_tokens", tokens.reasoning),
			slog.Float64("kaiak.usage.cost_usd", costUSD(cost)))
		// The media units only when not 0, as their metrics are fed.
		for _, f := range mediaUsageFields {
			if n := units[f.unit]; n != 0 {
				attrs = append(attrs, slog.Int64(f.key, n))
			}
		}
		attrs = append(attrs,
			slog.Bool("kaiak.usage.estimated", u.Estimated),
			slog.Bool("kaiak.usage.partial", u.Partial))
	}
	// The capture's outcome on the request's path; what its build drops is in the
	// counts alone, the line being written first. Never the session ID: client text.
	if rq.captureOutcome != "" {
		attrs = append(attrs, slog.String("kaiak.capture", rq.captureOutcome))
	}
	if rq.capture != nil && rq.capture.SessionIgnored() != "" {
		attrs = append(attrs, slog.String("kaiak.session_id.ignored", rq.capture.SessionIgnored()))
	}
	a.logger.LogAttrs(rq.r.Context(), slog.LevelInfo, "request", attrs...)
}

// tokenCounts are a request's tokens as the GenAI convention counts them, the
// request line's and its capture's: input is all input — config.InputUnits, the
// input neither read from nor written to the cache plus its two cache parts — and
// output all output, its reasoning part included.
type tokenCounts struct {
	input, cacheRead, cacheWrite, output, reasoning int64
}

// tokensOf is the token counts of a request's units (request.totalUsage).
func tokensOf(units accounting.Units) tokenCounts {
	return tokenCounts{input: units.Sum(config.InputUnits), cacheRead: units[config.UnitTokensCached],
		cacheWrite: units[config.UnitTokensCacheWrite], output: units[config.UnitTokensOut],
		reasoning: units[config.UnitTokensReasoning]}
}

// costUSD is a cost in nano-USD in dollars, for reading.
func costUSD(nanoUSD int64) float64 {
	return float64(nanoUSD) / 1e9
}

// mediaUsageFields are the request line's media usage fields, in line order: each
// media unit under the name of the metric counting it (docs/specs/GATEWAY.md,
// Observability → Logs: the request line).
var mediaUsageFields = []struct {
	key  string
	unit config.Unit
}{
	{"kaiak.usage.images", config.UnitImages},
	{"kaiak.usage.audio.output.duration", config.UnitAudioSecondsOut},
	{"kaiak.usage.audio.input.duration", config.UnitAudioSecondsIn},
}

// statusSent is the status answered to rq; false for a client that left before any
// answer: none was sent, and its error's 499 never reached the wire
// (docs/specs/GATEWAY.md, Observability → Logs and the request duration's
// attributes).
func statusSent(rq *request) (int, bool) {
	if rq.failure != nil && rq.failure.code == codeClientClosed {
		return 0, false
	}
	return rq.w.status, true
}

// methodAttrs are the request method's log fields: http.request.method (knownMethod)
// and, for _OTHER, http.request.method_original, the method as sent (clipped).
func methodAttrs(method string) []slog.Attr {
	known := knownMethod(method)
	if known != methodOther {
		return []slog.Attr{slog.String("http.request.method", known)}
	}
	return []slog.Attr{slog.String("http.request.method", known),
		slog.String("http.request.method_original", clip.String(method))}
}

// methodOther is http.request.method for a method the HTTP semantic convention does
// not name.
const methodOther = "_OTHER"

// knownMethod is the request method as http.request.method has it: a method the
// HTTP semantic convention names, else _OTHER — never the method as sent.
func knownMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodConnect, http.MethodTrace, "QUERY":
		return method
	}
	return methodOther
}

// triedAttempts lists a request's attempts for the log line, in order, as
// backend/deployment_model:outcome — the backend's status or the gateway's error
// code (e.g. "down/m:upstream_unavailable,local/m:200").
func triedAttempts(rq *request) string {
	var b strings.Builder
	for i, at := range rq.attempts {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(at.deployment.Backend.ID + "/" + at.deployment.Model + ":" + attemptOutcome(at))
	}
	return b.String()
}

// attemptOutcome is what attempt at got, for the log line: the backend's status, else
// the gateway's error code.
func attemptOutcome(at *attempt) string {
	if at.status != 0 {
		return strconv.Itoa(at.status)
	}
	if at.failure != nil {
		return at.failure.code
	}
	return ""
}
