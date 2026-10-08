package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// The upstream fields are the answering attempt's: an attempt that was retried
	// shows only in kaiak.tried.
	at := rq.answeringAttempt()
	if b := at.deployment.Backend; b != nil {
		attrs = append(attrs, slog.String("kaiak.backend.id", b.ID),
			slog.String("kaiak.backend.type", string(b.Type)))
		if name := provider.ProviderName(b.Type); name != "" {
			attrs = append(attrs, slog.String("gen_ai.provider.name", name))
		}
		attrs = append(attrs, slog.String("kaiak.deployment.model", at.deployment.Model),
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
		// gen_ai.usage.input_tokens is all input, as the GenAI convention means it:
		// config.InputUnits, the input neither read from nor written to the cache plus
		// its two cache parts.
		attrs = append(attrs,
			slog.Int64("gen_ai.usage.input_tokens", units.Sum(config.InputUnits)),
			slog.Int64("gen_ai.usage.cache_read.input_tokens", units[config.UnitTokensCached]),
			slog.Int64("gen_ai.usage.cache_write.input_tokens", units[config.UnitTokensCacheWrite]),
			slog.Int64("gen_ai.usage.output_tokens", units[config.UnitTokensOut]),
			slog.Int64("gen_ai.usage.reasoning.output_tokens", units[config.UnitTokensReasoning]),
			slog.Float64("kaiak.usage.cost_usd", float64(cost)/1e9),
			slog.Bool("kaiak.usage.estimated", u.Estimated),
			slog.Bool("kaiak.usage.partial", u.Partial))
	}
	a.logger.LogAttrs(rq.r.Context(), slog.LevelInfo, "request", attrs...)
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
