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
//     one token.
//
// One pass over the body, bounded by its size. A body that does not scan (the parse
// before this refuses those) is estimated by its size alone.
func EstimateInput(ep provider.Endpoint, body []byte) InputEstimate {
	whole := EstimateTokens(int64(len(body)))
	fallback := InputEstimate{Total: whole, LargestPrompt: whole}
	s := &inputScan{dec: json.NewDecoder(bytes.NewReader(body))}
	s.dec.UseNumber()

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
