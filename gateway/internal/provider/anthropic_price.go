package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The Anthropic types bill some Messages options above the standard rates the price
// table holds; both refuse them before sending (docs/specs/GATEWAY.md, Providers →
// Standard price on Anthropic types): fast mode (speed other than "standard"),
// US-only inference (inference_geo other than "global") and 1-hour cache writes (a
// cache_control with ttl "1h"). Keys are matched exactly, as the inbound stage reads
// them; a null value counts as absent.

// codePriceOptionUnsupported is the refusal's code (docs/specs/GATEWAY.md, Client API).
const codePriceOptionUnsupported = "price_option_unsupported"

// refusePriceOptions returns the refusal of a Messages body asking for a price
// option, nil when it asks for none. The inbound stage has checked the body is a
// JSON object (and so nests within encoding/json's limit); a member of an unexpected
// shape is the backend's to judge. One streaming pass over the body: nothing is
// copied, whatever the size of its content.
func refusePriceOptions(body []byte) error {
	s := priceScan{dec: json.NewDecoder(bytes.NewReader(body))}
	if tok, err := s.dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	for s.dec.More() {
		name, err := s.memberName()
		if err != nil {
			return nil
		}
		var refusal *RefusalError
		switch name {
		case "speed":
			refusal, err = s.standardOnly("speed", "standard", "'speed' asks for a mode billed above the standard price, "+
				`which the gateway does not price; send "standard" or leave it out.`)
		case "inference_geo":
			refusal, err = s.standardOnly("inference_geo", "global", "'inference_geo' asks for an inference location "+
				`billed above the standard price, which the gateway does not price; send "global" or leave it out.`)
		case "cache_control":
			refusal, err = s.cacheControl("cache_control")
		case "system", "messages", "tools":
			refusal, err = s.value(name, true)
		default:
			refusal, err = s.value(name, false)
		}
		if refusal != nil {
			return refusal
		}
		if err != nil {
			return nil
		}
	}
	return nil
}

// priceScan reads a Messages body token by token for the price options it carries.
type priceScan struct {
	dec *json.Decoder
}

func (s *priceScan) memberName() (string, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return "", err
	}
	name, _ := tok.(string) // inside an object, More() true means a name comes next
	return name, nil
}

// standardOnly reads a top-level option whose only standard value is want: absent,
// null or want passes; any other value is refused with message.
func (s *priceScan) standardOnly(param, want, message string) (*RefusalError, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return nil, err
	}
	if v, isString := tok.(string); tok == nil || (isString && v == want) {
		return nil, nil
	}
	return priceRefusal(param, message), nil
}

// value reads the value at path. With check, every cache_control in it — at any
// depth: a tool result's content, a document's content source, a search result's
// content — is read for a 1-hour cache write, and a cache_control named twice in one
// object is refused.
func (s *priceScan) value(path string, check bool) (*RefusalError, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		seen := false
		for s.dec.More() {
			name, err := s.memberName()
			if err != nil {
				return nil, err
			}
			at := path + "." + name
			var refusal *RefusalError
			if check && name == "cache_control" {
				if seen {
					return duplicateRefusal(at), nil
				}
				seen = true
				refusal, err = s.cacheControl(at)
			} else {
				refusal, err = s.value(at, check)
			}
			if refusal != nil || err != nil {
				return refusal, err
			}
		}
		_, err = s.dec.Token()
		return nil, err
	case json.Delim('['):
		for i := 0; s.dec.More(); i++ {
			if refusal, err := s.value(fmt.Sprintf("%s[%d]", path, i), check); refusal != nil || err != nil {
				return refusal, err
			}
		}
		_, err = s.dec.Token()
		return nil, err
	}
	return nil, nil
}

// cacheControl reads a cache_control value at path: a ttl of "1h" is refused, and so
// is a ttl named twice.
func (s *priceScan) cacheControl(path string) (*RefusalError, error) {
	tok, err := s.dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		if d, isDelim := tok.(json.Delim); isDelim && d == '[' {
			return nil, s.rest()
		}
		return nil, nil
	}
	seenTTL := false
	for s.dec.More() {
		name, err := s.memberName()
		if err != nil {
			return nil, err
		}
		if name != "ttl" {
			if _, err := s.value(path+"."+name, false); err != nil {
				return nil, err
			}
			continue
		}
		if seenTTL {
			return duplicateRefusal(path + ".ttl"), nil
		}
		seenTTL = true
		tok, err := s.dec.Token()
		if err != nil {
			return nil, err
		}
		if ttl, isString := tok.(string); isString && ttl == "1h" {
			return priceRefusal(path+".ttl", fmt.Sprintf("'%s.ttl' asks for a 1-hour cache write, billed above the standard price, "+
				`which the gateway does not price; use the 5-minute cache (leave ttl out).`, path)), nil
		} else if d, isDelim := tok.(json.Delim); isDelim && (d == '{' || d == '[') {
			if err := s.rest(); err != nil {
				return nil, err
			}
		}
	}
	_, err = s.dec.Token()
	return nil, err
}

// rest reads the rest of a container whose opening delimiter was just read.
func (s *priceScan) rest() error {
	for depth := 1; depth > 0; {
		tok, err := s.dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

func priceRefusal(param, message string) *RefusalError {
	return &RefusalError{Code: codePriceOptionUnsupported, Param: param, Message: message}
}

// duplicateRefusal refuses a member named twice in one object, which the gateway and
// the backend could read differently (the inbound stage's duplicate_member).
func duplicateRefusal(param string) *RefusalError {
	return &RefusalError{Code: "duplicate_member", Param: param,
		Message: fmt.Sprintf("'%s' is given more than once; send it once.", param)}
}
