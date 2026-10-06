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
// JSON object; a member of an unexpected shape is the backend's to judge.
func refusePriceOptions(body []byte) error {
	top := objectMembers(body)
	if !absentOr(top["speed"], "standard") {
		return priceRefusal("speed", "'speed' asks for a mode billed above the standard price, which the gateway does not price; "+
			`send "standard" or leave it out.`)
	}
	if !absentOr(top["inference_geo"], "global") {
		return priceRefusal("inference_geo", "'inference_geo' asks for an inference location billed above the standard price, "+
			`which the gateway does not price; send "global" or leave it out.`)
	}
	if param, ok := oneHourCacheWrite(top); ok {
		return priceRefusal(param, fmt.Sprintf("'%s' asks for a 1-hour cache write, billed above the standard price, "+
			`which the gateway does not price; use the 5-minute cache (leave ttl out).`, param))
	}
	return nil
}

func priceRefusal(param, message string) *RefusalError {
	return &RefusalError{Code: codePriceOptionUnsupported, Param: param, Message: message}
}

// absentOr reports whether a member is absent, null, or the string want.
func absentOr(raw json.RawMessage, want string) bool {
	if raw == nil || string(raw) == "null" {
		return true
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == want
}

// oneHourCacheWrite finds a cache_control asking for a 1-hour cache write anywhere a
// Messages request can carry one — the top level, system blocks, message content
// blocks, tool definitions — and names its ttl as a parameter path.
func oneHourCacheWrite(top map[string]json.RawMessage) (param string, found bool) {
	if oneHour(top["cache_control"]) {
		return "cache_control.ttl", true
	}
	for i, block := range arrayItems(top["system"]) {
		if oneHour(objectMembers(block)["cache_control"]) {
			return fmt.Sprintf("system[%d].cache_control.ttl", i), true
		}
	}
	for i, message := range arrayItems(top["messages"]) {
		for j, block := range arrayItems(objectMembers(message)["content"]) {
			if oneHour(objectMembers(block)["cache_control"]) {
				return fmt.Sprintf("messages[%d].content[%d].cache_control.ttl", i, j), true
			}
		}
	}
	for i, tool := range arrayItems(top["tools"]) {
		if oneHour(objectMembers(tool)["cache_control"]) {
			return fmt.Sprintf("tools[%d].cache_control.ttl", i), true
		}
	}
	return "", false
}

// oneHour reports whether a cache_control value sets ttl "1h".
func oneHour(cacheControl json.RawMessage) bool {
	var ttl string
	return json.Unmarshal(objectMembers(cacheControl)["ttl"], &ttl) == nil && ttl == "1h"
}

// objectMembers decodes a JSON object's members by exact key; nil for anything else
// (a string system prompt, a string message content).
func objectMembers(raw json.RawMessage) map[string]json.RawMessage {
	var members map[string]json.RawMessage
	if !startsWith(raw, '{') || json.Unmarshal(raw, &members) != nil {
		return nil
	}
	return members
}

// arrayItems decodes a JSON array's items; nil for anything else.
func arrayItems(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if !startsWith(raw, '[') || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

// startsWith reports whether a JSON value, past leading whitespace, begins with c.
func startsWith(raw json.RawMessage, c byte) bool {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	return len(raw) > 0 && raw[0] == c
}
