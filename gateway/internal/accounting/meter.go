package accounting

import (
	"encoding/json"
	"math"
	"sync/atomic"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// Units maps usage units to amounts (docs/specs/CONTROL-PROTOCOL.md, Prices). Every
// record from a token endpoint carries every token unit (config.TokenUnits), zeros
// included.
type Units map[config.Unit]int64

// withEveryTokenUnit is units with every token unit (config.TokenUnits) it leaves out
// added at 0.
func withEveryTokenUnit(units Units) Units {
	for _, unit := range config.TokenUnits {
		if _, ok := units[unit]; !ok {
			units[unit] = 0
		}
	}
	return units
}

// Sum is the total of units' amounts of set, saturating at math.MaxInt64 (amounts are
// never negative).
func (units Units) Sum(set []config.Unit) int64 {
	var sum int64
	for _, unit := range set {
		sum = SaturatingAdd(sum, units[unit])
	}
	return sum
}

// SaturatingAdd is a + b for non-negative a and b, math.MaxInt64 when that overflows:
// an amount never wraps.
func SaturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

// bytesPerToken is the estimation heuristic: about 4 bytes of text per token. There is
// no tokenizer — that would need a dependency, and per-model vocabularies.
const bytesPerToken = 4

// EstimateTokens estimates the tokens in n bytes of text (docs/specs/GATEWAY.md,
// Limits and Accounting), rounding up. EstimateInput applies it to a request's text.
func EstimateTokens(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + bytesPerToken - 1) / bytesPerToken
}

// Limits on the non-stream body members kept for settlement. A usage object is a few
// hundred bytes; the generated content (OpenAI's choices, a Messages content list) is
// kept only to estimate output when usage is missing, and past the limit the estimate
// falls back to its raw size.
const (
	maxUsageBytes   = 64 << 10
	maxContentBytes = 4 << 20
)

// Meter watches one routed attempt's response on its way to the client and works out
// its usage when it is over. It reads responses in the client's format, which every
// provider hands back: its endpoint's format reader (usageReader) knows where that
// format reports usage and carries generated content. A Meter belongs to one attempt
// and is used from that request's goroutine only — except Sent, which the transport
// calls.
type Meter struct {
	// input is the request's input estimate (EstimateInput), billed when the backend
	// reports no usage.
	input int64
	// reader reads the endpoint's format.
	reader usageReader

	// sent: the upstream request was written to the backend in full.
	sent atomic.Bool

	answered bool
	status   int
	stream   bool
	// refused: the backend answered by refusing the gateway's credential.
	refused bool

	// body scans a successful non-stream body.
	body *memberScanner
}

// usageReader reads one client API format's responses for the meter
// (docs/specs/GATEWAY.md, Accounting): where the backend reports usage, and where the
// generated content an estimate counts lives.
type usageReader interface {
	// streamEvent reads one stream event's payload as the backend sent it.
	streamEvent(payload []byte)
	// contentMember is the non-stream body member holding the generated content.
	contentMember() string
	// bodyUsage reads a non-stream body's usage member.
	bodyUsage(raw json.RawMessage)
	// bodyContentBytes counts the generated content in the body's content member.
	bodyContentBytes(raw json.RawMessage) int64
	// reported is the latest usage the backend reported, nil when none, and whether
	// its output count is final — a format whose stream reports a provisional output
	// count first (Messages) says false until the final one arrived.
	reported() (units Units, outputFinal bool)
	// streamContentBytes is the generated content seen in stream events.
	streamContentBytes() int64
}

// NewMeter returns the meter for a request to ep whose input is estimated at input
// tokens (EstimateInput).
func NewMeter(ep provider.Endpoint, input int64) *Meter {
	var reader usageReader
	switch ep.Format() {
	case provider.FormatMessages:
		reader = &messagesUsage{}
	case provider.FormatResponses:
		reader = &responsesUsage{}
	default:
		reader = &openAIUsage{endpoint: ep}
	}
	return &Meter{input: input, reader: reader}
}

// Answered records that the backend answered with status; stream reports whether the
// body is an event stream.
func (m *Meter) Answered(status int, stream bool) {
	m.answered = true
	m.status = status
	m.stream = stream
	if !stream && m.succeeded() {
		m.body = newMemberScanner(map[string]int{"usage": maxUsageBytes, m.reader.contentMember(): maxContentBytes})
	}
}

// Sent records that the upstream request was written to the backend in full
// (provider.Request.Sent); safe to call from any goroutine. From then on the backend
// may process the prompt — and bill it, upstream — so an attempt ending before any
// answer counts the input, estimated.
func (m *Meter) Sent() {
	m.sent.Store(true)
}

// Refused records that the backend refused the request outright — the gateway's
// credential, a model it does not serve, a path it does not have, a stream it gave up
// before its first event: an answer, with nothing processed.
func (m *Meter) Refused() {
	m.refused = true
}

// SentUnanswered reports whether the request reached the backend in full and got no
// answer (a first-event or response timeout, the connection lost, the client gone or the
// drain's cut before the first event): the case Settle bills estimated input for.
func (m *Meter) SentUnanswered() bool {
	return m.sent.Load() && !m.answered && !m.refused
}

// Observe reads one response event, hidden ones (the usage chunk the client did not
// ask for) included. Stream events are read from their payload as the backend sent
// it; body pieces from the bytes relayed.
func (m *Meter) Observe(ev provider.Event) {
	if !m.succeeded() {
		return
	}
	if m.stream {
		if len(ev.Payload) > 0 {
			m.reader.streamEvent(ev.Payload)
		}
		return
	}
	if m.body != nil {
		m.body.feed(ev.Data)
	}
}

// StreamContentSeen reports whether a successful stream has carried generated content
// yet (text, reasoning, refusal or tool calls — not role-only or usage chunks): the
// moment it turns true is the stream's first token.
func (m *Meter) StreamContentSeen() bool {
	return m.reader.streamContentBytes() > 0
}

func (m *Meter) succeeded() bool {
	return m.status >= 200 && m.status < 300
}

// Flags qualify a record's units.
type Flags struct {
	// Estimated: the backend reported no usage; units come from the byte heuristic.
	Estimated bool
	// Partial: the request stopped before its response ended (client disconnect,
	// backend failure); units count what was produced up to then.
	Partial bool
}

// Settle works out the attempt's units once it is over. complete reports whether the
// response ran to its end.
//
//   - Sent in full and no answer (SentUnanswered: a first-event or response timeout, the
//     connection lost, the client gone or the drain's cut before the first event):
//     the input estimated from the client's request body, no output; flagged
//     estimated and partial.
//   - No answer otherwise (connection failure, the request not written in full, a
//     refused credential): the backend never had the prompt, or processed nothing —
//     no units; the record says partial and carries zeros.
//   - A backend error status: the backend generated nothing; no units.
//   - Otherwise the backend's usage report when there is one. When its output count
//     is only provisional (a Messages stream ended before its message_delta usage),
//     output is estimated from the generated content seen, never below the provisional
//     count, and the record is flagged estimated.
//   - With no report, input is estimated from the client's request body and output
//     from the generated content seen, and the record is flagged estimated.
func (m *Meter) Settle(complete bool) (Units, Flags) {
	if m.SentUnanswered() {
		return withEveryTokenUnit(Units{config.UnitTokensIn: m.input}), Flags{Estimated: true, Partial: true}
	}
	flags := Flags{Partial: !complete || !m.answered}
	if !m.answered || !m.succeeded() {
		return withEveryTokenUnit(Units{}), flags
	}
	if !m.stream && m.body != nil {
		if raw, ok := m.body.member("usage"); ok {
			m.reader.bodyUsage(raw)
		}
	}
	if units, outputFinal := m.reader.reported(); units != nil {
		if !outputFinal {
			units[config.UnitTokensOut] = max(units[config.UnitTokensOut], EstimateTokens(m.outputBytes()))
			flags.Estimated = true
		}
		return units, flags
	}
	flags.Estimated = true
	return withEveryTokenUnit(Units{
		config.UnitTokensIn:  m.input,
		config.UnitTokensOut: EstimateTokens(m.outputBytes()),
	}), flags
}

// outputBytes is the generated content seen: stream content, or the content member of
// a non-stream body (its raw size when it could not be kept whole).
func (m *Meter) outputBytes() int64 {
	if m.stream || m.body == nil {
		return m.reader.streamContentBytes()
	}
	member := m.reader.contentMember()
	raw, ok := m.body.member(member)
	if !ok {
		return int64(m.body.memberSize(member))
	}
	return m.reader.bodyContentBytes(raw)
}

// inclusiveUnits maps a usage report whose input count includes the tokens read from
// and written to the cache, and whose output count includes reasoning (OpenAI's and
// Responses' usage), onto the units (docs/specs/CONTROL-PROTOCOL.md, Units and price
// units): tokens_cached = cached, tokens_cache_write = written, tokens_in = input −
// cached − written, tokens_out = output (reasoning included), tokens_reasoning = the
// reasoning share of it. Cached is clamped to input first, then written to what
// remains, so the three input units add up to input; reasoning is clamped to output.
// A report with neither count is no report.
func inclusiveUnits(input, output *int64, cached, written, reasoning int64) (Units, bool) {
	if input == nil && output == nil {
		return nil, false
	}
	in, out := nonNegative(input), nonNegative(output)
	cached = min(max(cached, 0), in)
	written = min(max(written, 0), in-cached)
	reasoning = min(max(reasoning, 0), out)
	return withEveryTokenUnit(Units{
		config.UnitTokensIn:         in - cached - written,
		config.UnitTokensCached:     cached,
		config.UnitTokensCacheWrite: written,
		config.UnitTokensOut:        out,
		config.UnitTokensReasoning:  reasoning,
	}), true
}

// latestReport is the usage of a format whose every report is whole (OpenAI,
// Responses): the latest replaces any before it, and its output count is final.
type latestReport struct {
	// latest is the latest usage the backend reported; nil when none yet.
	latest Units
}

// keep makes units the latest report when ok.
func (r *latestReport) keep(units Units, ok bool) {
	if ok {
		r.latest = units
	}
}

func (r *latestReport) reported() (Units, bool) { return r.latest, true }

func nonNegative(n *int64) int64 {
	if n == nil || *n < 0 {
		return 0
	}
	return *n
}
