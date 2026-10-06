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
		tokenIDs := ep == provider.Embeddings && key == "input"
		if _, ok := s.value(key, "", tokenIDs, 1, &rest); !ok {
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
	return InputEstimate{
		Total:         all.tokens(),
		LargestPrompt: part{text: rest.text + largest.text, fixed: rest.fixed + largest.fixed}.tokens(),
	}
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
		if !s.handle(tok, s.dec.InputOffset()-start, "prompt", "", true, 1, &one) {
			return nil, false
		}
		return []part{s.measured(start, one)}, true
	}
	var elems []part
	flat := false
	for s.dec.More() {
		elemStart := s.dec.InputOffset()
		var p part
		first, ok := s.value("prompt", "", true, 2, &p)
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

// value scans one JSON value: the member key of an object that is itself member
// parent (array elements carry their array's key and parent). Bytes it leaves out of
// the text are subtracted from p.text; tokens it counts directly are added to
// p.fixed. It returns the value's first token.
func (s *inputScan) value(key, parent string, tokenIDs bool, depth int, p *part) (json.Token, bool) {
	if s.format == provider.FormatMessages && key == "source" {
		return nil, s.messagesSource(depth, p)
	}
	start := s.dec.InputOffset()
	tok, err := s.dec.Token()
	if err != nil {
		return nil, false
	}
	return tok, s.handle(tok, s.dec.InputOffset()-start, key, parent, tokenIDs, depth, p)
}

// handle scans the value whose first token tok, span bytes of the body, was just
// read (see value).
func (s *inputScan) handle(tok json.Token, span int64, key, parent string, tokenIDs bool, depth int, p *part) bool {
	switch v := tok.(type) {
	case json.Delim:
		if depth > maxEstimateDepth {
			return false
		}
		if v == '[' && s.format == provider.FormatResponses && key == "content" {
			return s.responsesParts(depth, p)
		}
		for s.dec.More() {
			member, enclosing := key, parent
			if v == '{' {
				name, err := s.dec.Token()
				if err != nil {
					return false
				}
				member, _ = name.(string)
				enclosing = key
			}
			if _, ok := s.value(member, enclosing, tokenIDs, depth+1, p); !ok {
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

// messagesSource scans a Messages block's source (docs/specs/GATEWAY.md, Limits: the
// input estimate): base64 data, a URL or a file ID is one media item, whatever its
// size; a text or content source — or a source that is not an object — is scanned as
// any other value. The source is read whole first: its type may come after its data.
func (s *inputScan) messagesSource(depth int, p *part) bool {
	start := s.dec.InputOffset()
	var raw json.RawMessage
	if err := s.dec.Decode(&raw); err != nil {
		return false
	}
	span := s.dec.InputOffset() - start
	var source struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &source) == nil {
		switch source.Type {
		case "base64", "url", "file":
			p.text -= span
			p.fixed += InlineMediaTokens
			return true
		}
	}
	inner := newInputScan(raw, provider.FormatMessages)
	tok, err := inner.dec.Token()
	if err != nil {
		return false
	}
	return inner.handle(tok, inner.dec.InputOffset(), "", "source", false, depth, p)
}

// responsesParts scans the rest of a Responses content list, its opening bracket
// read (docs/specs/GATEWAY.md, Limits: the input estimate): an input_image or
// input_file part is one media item, whatever it carries — a data URL, a URL or a
// file ID; any other part is scanned as any other value. Each part is read whole
// first: its type may come after its data.
func (s *inputScan) responsesParts(depth int, p *part) bool {
	for s.dec.More() {
		start := s.dec.InputOffset()
		var raw json.RawMessage
		if err := s.dec.Decode(&raw); err != nil {
			return false
		}
		span := s.dec.InputOffset() - start
		var part struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &part) == nil && (part.Type == "input_image" || part.Type == "input_file") {
			p.text -= span
			p.fixed += InlineMediaTokens
			continue
		}
		inner := newInputScan(raw, provider.FormatResponses)
		tok, err := inner.dec.Token()
		if err != nil {
			return false
		}
		if !inner.handle(tok, inner.dec.InputOffset(), "content", "", false, depth+1, p) {
			return false
		}
	}
	_, err := s.dec.Token()
	return err == nil
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
