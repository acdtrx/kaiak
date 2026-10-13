package capture

import (
	"cmp"
	"net/url"
	"strconv"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
	"kaiak/internal/telemetry/otlptrace"
)

// Summary is how a captured request ended, as the server knows it once the request
// is over: every attribute of its span but the content (docs/specs/GATEWAY.md,
// Request capture → Attributes), read as the request line reads it. Reading it
// costs the request nothing but copying values.
type Summary struct {
	// Start is the request's arrival; End the end of its response.
	Start, End time.Time
	RequestID  string
	KeyID      string
	// Group is the key's group's ID; RootGroup its top-level group's — the key's
	// group itself when that is top-level.
	Group, RootGroup string
	// Model is the model the client asked for, clipped as on the request line.
	Model string
	// Stream: the request asked for a stream.
	Stream bool
	// Status is the status the client was answered.
	Status int
	// Backend is the last attempt's backend, nil for a request never routed;
	// DeploymentModel is the model's name there, Attempts the attempts made.
	Backend         *config.Backend
	DeploymentModel string
	Attempts        int
	// ErrorType is the request line's error.type, "" for a request that did not end
	// in an error; RelayEnd its kaiak.relay_end, "" for a response that did not stop
	// early.
	ErrorType, RelayEnd string
	// ContentStreamed: a stream carried generated content, TimeToFirstToken after
	// the answering attempt was sent.
	ContentStreamed  bool
	TimeToFirstToken time.Duration
	// Usage is the request's settled usage; nil for a request never routed.
	Usage *Usage
}

// Usage is a request's usage as the request line carries it, summed over its
// records: all input with its two cache parts, all output with its reasoning part,
// the cost in dollars, and the last record's flags.
type Usage struct {
	InputTokens, CacheReadTokens, CacheWriteTokens int64
	OutputTokens, ReasoningTokens                  int64
	CostUSD                                        float64
	Estimated, Partial                             bool
}

// operationName is a capture's gen_ai.operation.name: every captured endpoint is a
// chat operation.
const operationName = "chat"

// span is j's span: one span of a trace of its own, from the request's arrival to
// the end of its response, named and with the attributes the spec's table gives
// (docs/specs/GATEWAY.md, Request capture → The span, Attributes) — the content's
// read from the copies by readContent, the rest from the summary. Its status is an
// error, with the span's error.type as its message, when it has one.
func (j job) span(readContent func(provider.Endpoint, exchange) []otlptrace.Attribute) otlptrace.Span {
	s := j.summary
	attrs := []otlptrace.Attribute{otlptrace.String("gen_ai.operation.name", operationName)}
	if b := s.Backend; b != nil {
		attrs = append(attrs, otlptrace.String("gen_ai.provider.name", provider.ProviderName(b.Type)))
	}
	attrs = append(attrs, otlptrace.String("gen_ai.request.model", s.Model))
	if s.Stream {
		attrs = append(attrs, otlptrace.Bool("gen_ai.request.stream", true))
	}
	if u := s.Usage; u != nil {
		attrs = append(attrs,
			otlptrace.Int("gen_ai.usage.input_tokens", u.InputTokens),
			otlptrace.Int("gen_ai.usage.cache_read.input_tokens", u.CacheReadTokens),
			otlptrace.Int("gen_ai.usage.cache_write.input_tokens", u.CacheWriteTokens),
			otlptrace.Int("gen_ai.usage.output_tokens", u.OutputTokens),
			otlptrace.Int("gen_ai.usage.reasoning.output_tokens", u.ReasoningTokens))
	}
	if j.session != "" {
		attrs = append(attrs, otlptrace.String("gen_ai.conversation.id", j.session))
	}
	if b := s.Backend; b != nil {
		if host, port, ok := serverAddress(b.BaseURL); ok {
			attrs = append(attrs, otlptrace.String("server.address", host), otlptrace.Int("server.port", port))
		}
	}
	// The value kaiak.request.errors carries: a response that stopped early is an
	// error too.
	errorType := cmp.Or(s.ErrorType, s.RelayEnd)
	if errorType != "" {
		attrs = append(attrs, otlptrace.String("error.type", errorType))
	}
	attrs = append(attrs, readContent(j.api, exchange{request: j.request, response: j.response, stream: s.Stream,
		status: s.Status})...)
	attrs = append(attrs,
		otlptrace.String("kaiak.request.id", s.RequestID),
		otlptrace.String("kaiak.key.id", s.KeyID),
		otlptrace.String("kaiak.key.group", s.Group),
		otlptrace.String("kaiak.key.root_group", s.RootGroup))
	if b := s.Backend; b != nil {
		attrs = append(attrs,
			otlptrace.String("kaiak.backend.id", b.ID),
			otlptrace.String("kaiak.backend.type", string(b.Type)),
			otlptrace.String("kaiak.deployment.model", s.DeploymentModel),
			otlptrace.Int("kaiak.attempts", int64(s.Attempts)))
	}
	if s.ContentStreamed {
		// Seconds to the microsecond, as on the request line.
		attrs = append(attrs, otlptrace.Float64("kaiak.time_to_first_token", float64(s.TimeToFirstToken.Microseconds())/1e6))
	}
	if s.RelayEnd != "" {
		attrs = append(attrs, otlptrace.String("kaiak.relay_end", s.RelayEnd))
	}
	if u := s.Usage; u != nil {
		attrs = append(attrs,
			otlptrace.Float64("kaiak.usage.cost_usd", u.CostUSD),
			otlptrace.Bool("kaiak.usage.estimated", u.Estimated),
			otlptrace.Bool("kaiak.usage.partial", u.Partial))
	}
	if j.sessionIgnored != "" {
		attrs = append(attrs, otlptrace.String("kaiak.session_id.ignored", j.sessionIgnored))
	}

	span := otlptrace.Span{TraceID: otlptrace.NewTraceID(), SpanID: otlptrace.NewSpanID(),
		Name: operationName + " " + s.Model, Kind: otlptrace.KindClient, Start: s.Start, End: s.End,
		Attributes: attrs}
	if errorType != "" {
		span.Status = otlptrace.Status{Code: otlptrace.StatusError, Message: errorType}
	}
	return span
}

// serverAddress is a backend's server.address and server.port: its base URL's host,
// and its port — the URL's, else 443 for https and 80 for http. false for a URL it
// cannot read, which config validation does not let through.
func serverAddress(baseURL string) (string, int64, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return "", 0, false
	}
	if p := u.Port(); p != "" {
		port, err := strconv.ParseInt(p, 10, 64)
		return u.Hostname(), port, err == nil
	}
	switch u.Scheme {
	case "https":
		return u.Hostname(), 443, true
	case "http":
		return u.Hostname(), 80, true
	}
	return "", 0, false
}
