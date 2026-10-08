package accounting

import (
	"bytes"
	"encoding/json"
	"strings"

	"kaiak/internal/provider"
)

// InlineMediaTokens is the input estimate of one media item — an image, audio clip or
// file the request carries (docs/specs/GATEWAY.md, Limits: tokens before the request
// runs). Its encoded size says nothing about what a backend bills for it: an image
// costs a model-specific, dimension-dependent number of tokens whatever its
// compression, so one flat figure stands in for all of them.
const InlineMediaTokens = 1000

// InputEstimate is a request's input, estimated once from its body.
type InputEstimate struct {
	// Total is the whole request's input: what limits reserve and what an estimated
	// record bills.
	Total int64
	// LargestPrompt is the input one generated sequence sees: the whole request, less
	// every prompt of a completion batch but its largest. The context an output limit
	// must fit in belongs to each sequence, not to the batch.
	LargestPrompt int64
}

// maxEstimateDepth bounds the scan's nesting. The owned-field parse has already
// refused deeper documents (encoding/json stops at 10000 levels); past it the estimate
// falls back to the body size.
const maxEstimateDepth = 10000

// EstimateInput estimates the input tokens of a request to ep from its client body.
// Text counts by bytes (EstimateTokens) over the body as received; these count
// differently, their bytes left out of the text:
//
//   - a media item counts InlineMediaTokens: any string that is a data URL, an
//     image_url part's URL whatever its form (the backend fetches a remote image and
//     bills it all the same), and an input_audio part's data (raw base64);
//   - a token ID in a completion's prompt or an embeddings request's input counts
//     one token;
//   - in a Messages request, an image or document block's source counts
//     InlineMediaTokens when it is base64 data, a URL or a file ID (a text or content
//     source is text, its own content scanned by these rules);
//   - in a Responses request, an input_image or input_file content part counts
//     InlineMediaTokens whatever it carries (data URL, URL or file ID).
//
// A rerank request's total adds its query's own estimate once more for each document
// beyond the first: the backend scores every document paired with the query.
//
// One pass over the body, bounded by its size. A body that does not scan (the parse
// before this refuses those) is estimated by its size alone.
func EstimateInput(ep provider.Endpoint, body []byte) InputEstimate {
	whole := EstimateTokens(int64(len(body)))
	fallback := InputEstimate{Total: whole, LargestPrompt: whole}
	s := newInputScan(body, ep.Format())

	if tok, err := s.dec.Token(); err != nil || tok != json.Delim('{') {
		return fallback
	}
	// rest is the body outside a completion's prompts.
	rest := part{text: int64(len(body))}
	var prompts []part
	// query is a rerank request's query, estimated on its own; documents how many
	// documents the request holds.
	var query part
	var documents int64
	for s.dec.More() {
		name, err := s.dec.Token()
		if err != nil {
			return fallback
		}
		key, _ := name.(string)
		if ep == provider.Completions && key == "prompt" {
			list, ok := s.promptList()
			if !ok {
				return fallback
			}
			for _, p := range list {
				rest.text -= p.span
			}
			prompts = append(prompts, list...)
			continue
		}
		if ep == provider.Rerank && key == "query" {
			start := valueOffset(body, s.dec.InputOffset())
			var q part
			if _, ok := s.value(key, "", roleNone, false, 1, &q); !ok {
				return fallback
			}
			query = s.measured(start, q)
			rest.text += query.text - query.span
			rest.fixed += query.fixed
			continue
		}
		if ep == provider.Rerank && key == "documents" {
			var ok bool
			if documents, ok = s.documentList(&rest); !ok {
				return fallback
			}
			continue
		}
		tokenIDs := ep == provider.Embeddings && key == "input"
		if _, ok := s.value(key, "", s.topRole(key), tokenIDs, 1, &rest); !ok {
			return fallback
		}
	}
	if _, err := s.dec.Token(); err != nil {
		return fallback
	}

	// Text is estimated over the sum of its bytes, so a body with nothing counted
	// directly estimates as its size.
	all, largest := rest, part{}
	for _, p := range prompts {
		all.text += p.text
		all.fixed += p.fixed
		if p.tokens() > largest.tokens() {
			largest = p
		}
	}
	total := all.tokens()
	if documents > 1 {
		total = SaturatingAdd(total, SaturatingMul(documents-1, query.tokens()))
	}
	return InputEstimate{
		Total:         total,
		LargestPrompt: part{text: rest.text + largest.text, fixed: rest.fixed + largest.fixed}.tokens(),
	}
}

// valueOffset is where the value of the member whose name ends at off begins in body:
// past the colon and the whitespace around it, which the decoder reads only with the
// value.
func valueOffset(body []byte, off int64) int64 {
	for off < int64(len(body)) {
		switch body[off] {
		case ':', ' ', '\t', '\r', '\n':
			off++
		default:
			return off
		}
	}
	return off
}

// part is an estimate under way: text bytes and tokens counted directly. While a
// value is scanned, text holds minus the bytes left out of it (media, token IDs);
// measured adds the value's span.
type part struct {
	text, fixed int64
	// span is the part's bytes of the body, text and left out alike.
	span int64
}

func (p part) tokens() int64 {
	return EstimateTokens(p.text) + p.fixed
}

type inputScan struct {
	dec *json.Decoder
	// format is the body's client API format: a Messages request's media live in
	// block sources, a Responses request's in content parts.
	format provider.Format
}

func newInputScan(data []byte, format provider.Format) *inputScan {
	s := &inputScan{dec: json.NewDecoder(bytes.NewReader(data)), format: format}
	s.dec.UseNumber()
	return s
}

// measured completes p, scanned from the body offset start to the decoder's offset.
func (s *inputScan) measured(start int64, p part) part {
	p.span = s.dec.InputOffset() - start
	p.text += p.span
	return p
}

// promptList scans a completion's prompt into one part per prompt: a list of
// strings or of token-ID lists holds one prompt per element; a string, one token-ID
// list or anything else is one prompt (as the sequence count reads it).
func (s *inputScan) promptList() ([]part, bool) {
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return nil, false
	}
	var one part
	if tok != json.Delim('[') {
		if !s.handle(tok, s.dec.InputOffset()-start, "prompt", "", roleNone, true, 1, &one) {
			return nil, false
		}
		return []part{s.measured(start, one)}, true
	}
	var elems []part
	flat := false
	for s.dec.More() {
		elemStart := s.dec.InputOffset()
		var p part
		first, ok := s.value("prompt", "", roleNone, true, 2, &p)
		if !ok {
			return nil, false
		}
		if _, isNumber := first.(json.Number); isNumber {
			flat = true
		}
		one.text += p.text
		one.fixed += p.fixed
		elems = append(elems, s.measured(elemStart, p))
	}
	if _, err := s.dec.Token(); err != nil {
		return nil, false
	}
	if flat || len(elems) == 0 {
		return []part{s.measured(start, one)}, true
	}
	return elems, true
}

// documentList scans a rerank request's documents into p and returns how many it
// holds, as the cap counts them: one per element of a list, one for any other value.
func (s *inputScan) documentList(p *part) (int64, bool) {
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return 0, false
	}
	if tok != json.Delim('[') {
		return 1, s.handle(tok, s.dec.InputOffset()-start, "documents", "", roleNone, false, 1, p)
	}
	var n int64
	for s.dec.More() {
		if _, ok := s.value("documents", "", roleNone, false, 2, p); !ok {
			return 0, false
		}
		n++
	}
	_, err = s.dec.Token()
	return n, err == nil
}

// scanRole is where a value sits in a Messages or Responses body: media count as
// such only at the format's documented content paths (docs/specs/GATEWAY.md, Limits:
// the input estimate) — a tool's arguments or schema that merely look like a content
// part are text.
type scanRole int

const (
	roleNone scanRole = iota
	// Messages: the messages list, one message, a list of content blocks (a message's
	// content, the system prompt, a tool result's or search result's content, a
	// content source's content), one block, a block's source.
	roleMessages
	roleMessage
	roleBlocks
	roleBlock
	roleSource
	// Responses: the input items, one item, a list of content parts (a message's
	// content, a function or custom tool call's output), one part.
	roleItems
	roleItem
	roleParts
	rolePart
)

// topRole is the role of the body's top-level member key.
func (s *inputScan) topRole(key string) scanRole {
	switch {
	case s.format == provider.FormatMessages && key == "messages":
		return roleMessages
	case s.format == provider.FormatMessages && key == "system":
		return roleBlocks
	case s.format == provider.FormatResponses && key == "input":
		return roleItems
	}
	return roleNone
}

// elementRole is the role of an element of an array whose role is r.
func elementRole(r scanRole) scanRole {
	switch r {
	case roleMessages:
		return roleMessage
	case roleBlocks:
		return roleBlock
	case roleItems:
		return roleItem
	case roleParts:
		return rolePart
	}
	return roleNone
}

// memberRole is the role of member name of an object whose role is r.
func memberRole(r scanRole, name string) scanRole {
	switch {
	case r == roleMessage && name == "content", r == roleBlock && name == "content", r == roleSource && name == "content":
		return roleBlocks
	case r == roleBlock && name == "source":
		return roleSource
	case r == roleItem && (name == "content" || name == "output"):
		return roleParts
	}
	return roleNone
}

// value scans one JSON value, of role r: the member key of an object that is itself
// member parent (array elements carry their array's key and parent). Bytes it leaves
// out of the text are subtracted from p.text; tokens it counts directly are added to
// p.fixed. It returns the value's first token.
func (s *inputScan) value(key, parent string, r scanRole, tokenIDs bool, depth int, p *part) (json.Token, bool) {
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return nil, false
	}
	return tok, s.handle(tok, s.dec.InputOffset()-start, key, parent, r, tokenIDs, depth, p)
}

// handle scans the value whose first token tok, span bytes of the body, was just
// read (see value).
func (s *inputScan) handle(tok json.Token, span int64, key, parent string, r scanRole, tokenIDs bool, depth int, p *part) bool {
	switch v := tok.(type) {
	case json.Delim:
		if depth > maxEstimateDepth {
			return false
		}
		if v == '{' {
			_, ok := s.object(key, r, tokenIDs, depth, p)
			return ok
		}
		for s.dec.More() {
			if _, ok := s.value(key, parent, elementRole(r), tokenIDs, depth+1, p); !ok {
				return false
			}
		}
		_, err := s.dec.Token()
		return err == nil
	case string:
		if isMedia(v, key, parent) {
			p.text -= span
			p.fixed += InlineMediaTokens
		}
	case json.Number:
		if tokenIDs {
			p.text -= span
			p.fixed++
		}
	}
	return true
}

// object scans the rest of an object of role r, its opening brace just read, as
// member key, and returns its type member when that is a string. Every byte is read
// once, whatever the nesting (docs/specs/GATEWAY.md, Limits: the input estimate). A
// part's type may come after its data, so the object's own count is held apart until
// it closes:
//
//   - a Responses content part (a message's content, a tool call's output) of type
//     input_image or input_file counts InlineMediaTokens, whatever it carries (a data
//     URL, a URL or a file ID);
//   - a Messages content block of type image or document (in a message, a tool
//     result, a content source) counts its source as InlineMediaTokens when the
//     source is base64 data, a URL or a file ID.
//
// Exact keys only, as the inbound stage reads them; anywhere else such an object is
// text.
func (s *inputScan) object(key string, r scanRole, tokenIDs bool, depth int, p *part) (string, bool) {
	start := s.dec.InputOffset() - 1 // the opening brace
	var own, source part
	var typ, sourceType string
	var sourceSpan int64
	var ok bool
	for s.dec.More() {
		name, err := s.dec.Token()
		if err != nil {
			return "", false
		}
		member, _ := name.(string)
		if r == roleBlock && member == "source" {
			if sourceType, sourceSpan, ok = s.messagesSource(key, tokenIDs, depth+1, &source); !ok {
				return "", false
			}
			continue
		}
		first, valueOK := s.value(member, key, memberRole(r, member), tokenIDs, depth+1, &own)
		if !valueOK {
			return "", false
		}
		if t, isString := first.(string); isString && member == "type" {
			typ = t
		}
	}
	if _, err := s.dec.Token(); err != nil {
		return "", false
	}
	switch {
	case r == rolePart && (typ == "input_image" || typ == "input_file"):
		p.text -= s.dec.InputOffset() - start
		p.fixed += InlineMediaTokens
	case r == roleBlock && (typ == "image" || typ == "document") && isMediaSource(sourceType):
		own.text -= sourceSpan
		own.fixed += InlineMediaTokens
		p.text += own.text
		p.fixed += own.fixed
	default:
		p.text += own.text + source.text
		p.fixed += own.fixed + source.fixed
	}
	return typ, true
}

// messagesSource scans a Messages block's source, counted into p, and returns its type
// and its span when it is an object (an object's span is its own bytes).
func (s *inputScan) messagesSource(parent string, tokenIDs bool, depth int, p *part) (typ string, span int64, ok bool) {
	before := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return "", 0, false
	}
	if tok != json.Delim('{') {
		return "", 0, s.handle(tok, s.dec.InputOffset()-before, "source", parent, roleSource, tokenIDs, depth, p)
	}
	if depth > maxEstimateDepth {
		return "", 0, false
	}
	start := s.dec.InputOffset() - 1 // the opening brace
	if typ, ok = s.object("source", roleSource, tokenIDs, depth, p); !ok {
		return "", 0, false
	}
	return typ, s.dec.InputOffset() - start, true
}

// isMediaSource reports whether a Messages source of type t is media: base64 data, a
// URL or a file ID; a text or content source is text, its content scanned as any.
func isMediaSource(t string) bool {
	return t == "base64" || t == "url" || t == "file"
}

// isMedia reports whether the string s, the value of member key in an object that is
// itself member parent, is a media item: a data URL anywhere, an image_url part's
// URL (image_url.url, or image_url as a bare string) in any form, or an input_audio
// part's data.
func isMedia(s, key, parent string) bool {
	return isDataURL(s) ||
		(key == "url" && parent == "image_url") || key == "image_url" ||
		(key == "data" && parent == "input_audio")
}

// maxDataURLHeader bounds a data URL's header ("data:" up to the comma): a media type
// and parameters are short.
const maxDataURLHeader = 256

// isDataURL reports whether s is a data URL (RFC 2397): "data:", a header with no
// whitespace naming a media type (type/subtype) or base64, then a comma. Text that
// merely starts with "data:" — a pasted event-stream line — is not one.
func isDataURL(s string) bool {
	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return false
	}
	header, _, ok := strings.Cut(rest, ",")
	if !ok || len(header) > maxDataURLHeader || strings.ContainsAny(header, " \t\r\n") {
		return false
	}
	return strings.Contains(header, "/") || strings.HasSuffix(header, ";base64")
}
