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
	"kaiak/internal/limits"
	"kaiak/internal/logattr"
)

// logRequest writes the one log line per request, in the log vocabulary
// (docs/specs/GATEWAY.md, Observability: Logs → the request line). It never carries
// the key, a header value or any request or response content; strings the client
// controls (method, path, model) are clipped.
func (a *API) logRequest(rq *request) {
	attrs := []slog.Attr{slog.String("kaiak.request.id", rq.id)}
	attrs = append(attrs, methodAttrs(rq.r.Method)...)
	attrs = append(attrs, slog.String("url.path", clip.String(rq.r.URL.Path)))
	// No status for a client that left before any answer: none was sent. Its 499 is
	// the request metric's alone (docs/specs/GATEWAY.md, Observability → Logs).
	if rq.failure == nil || rq.failure.code != codeClientClosed {
		attrs = append(attrs, slog.Int("http.response.status_code", rq.w.status))
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
	if rq.modelAllowed && rq.endpoint.takesBody() {
		attrs = append(attrs, slog.Bool("gen_ai.request.stream", rq.inbound.Stream))
	}
	if op := rq.endpoint.operationName(); op != "" {
		attrs = append(attrs, slog.String("gen_ai.operation.name", op))
	}
	// The upstream fields are the answering attempt's: an attempt that was retried
	// shows only in kaiak.tried.
	at := rq.answeringAttempt()
	if b := at.deployment.Backend; b != nil {
		attrs = append(attrs, slog.String("kaiak.backend.id", b.ID),
			slog.String("kaiak.backend.type", string(b.Type)))
		if name := providerName(b.Type); name != "" {
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
		attrs = append(attrs, limitAttrs(rej)...)
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
		// Units and cost are the request's: every record's (one per attempt that
		// has usage); the flags are the answering attempt's.
		units := make(accounting.Units)
		var cost int64
		for _, rec := range rq.records {
			for unit, n := range rec.Units {
				units[unit] += n
			}
			cost += rec.CostNanoUSD
		}
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

// methodAttrs are the request method's log fields: http.request.method — a method
// the HTTP semantic convention names, else _OTHER — and, for _OTHER,
// http.request.method_original, the method as sent (clipped).
func methodAttrs(method string) []slog.Attr {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodConnect, http.MethodTrace, "QUERY":
		return []slog.Attr{slog.String("http.request.method", method)}
	}
	return []slog.Attr{slog.String("http.request.method", "_OTHER"),
		slog.String("http.request.method_original", clip.String(method))}
}

// providerName is gen_ai.provider.name for a backend type: the GenAI convention's
// well-known value where one fits, else "" — the self-hosted types have none, and
// kaiak.backend.type names every type. Claude in Foundry is Anthropic's service and
// API on Azure, and no Azure value names it, so it is anthropic too.
func providerName(t config.BackendType) string {
	switch t {
	case config.BackendOpenAI:
		return "openai"
	case config.BackendAzureOpenAI:
		return "azure.ai.openai"
	case config.BackendAnthropic, config.BackendAzureAnthropic:
		return "anthropic"
	}
	return ""
}

// limitAttrs are a limit refusal's log fields: the limit's kind of scope (global or
// group), its group's ID (absent for a global limit), its type, the value enforced (a
// per-minute limit's share among the live gateways) and the value configured, what the
// window had used (unknown for a budget refused as unavailable) and, for a token
// limit, what the request asked for. Counts are in the limit's unit; USD limits are in
// dollars, as kaiak.usage.cost_usd (limits.LogValue).
func limitAttrs(rej *limits.Rejection) []slog.Attr {
	value := func(key string, v int64) slog.Attr { return slog.Any(key, limits.LogValue(rej.Measure, v)) }
	attrs := []slog.Attr{slog.String("kaiak.limit.scope", string(rej.Scope))}
	if rej.Group != "" {
		attrs = append(attrs, slog.String("kaiak.limit.group", rej.Group))
	}
	attrs = append(attrs, slog.String("kaiak.limit.type", string(rej.Type)), value("kaiak.limit.enforced", rej.Limit),
		value("kaiak.limit.configured", rej.Max))
	if rej.Unavailable {
		return attrs
	}
	attrs = append(attrs, value("kaiak.limit.used", rej.Used))
	if rej.Measure == config.MeasureTokens {
		attrs = append(attrs, slog.Int64("kaiak.limit.requested", rej.Requested))
	}
	return attrs
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
