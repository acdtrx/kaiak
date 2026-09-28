package server

import (
	"bytes"
	"encoding/json"
)

// backendErrorReadMax bounds how much of a backend 5xx body is read to find its
// error code and type; a longer body is read no further.
const backendErrorReadMax = 4096

// backendFieldMax is the most bytes of a backend error code or type logged.
const backendFieldMax = 64

// backendErrorFields returns the error code and type a backend error body names —
// the OpenAI shape ({"error": {"code", "type", …}}) or the flat one vLLM answers
// ({"code", "type", …}) — each "" when absent. Only short identifiers are taken: a
// string of letters, digits and "_.:-" (or an integer code), at most backendFieldMax
// bytes kept. Anything else — above all the message, which can echo request
// content — is never taken (AGENTS.md: no prompt or response content in logs).
func backendErrorFields(body []byte) (code, typ string) {
	type fields struct {
		Error json.RawMessage `json:"error"`
		Code  json.RawMessage `json:"code"`
		Type  json.RawMessage `json:"type"`
	}
	var outer fields
	// The first JSON value only: a body cut at backendErrorReadMax may trail off.
	if json.NewDecoder(bytes.NewReader(body)).Decode(&outer) != nil {
		return "", ""
	}
	if len(outer.Error) == 0 || outer.Error[0] != '{' {
		return backendIdentifier(outer.Code), backendIdentifier(outer.Type)
	}
	var inner fields
	if json.Unmarshal(outer.Error, &inner) != nil {
		return "", ""
	}
	return backendIdentifier(inner.Code), backendIdentifier(inner.Type)
}

// backendIdentifier is raw as a short identifier, or "".
func backendIdentifier(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&n) != nil {
			return ""
		}
		if _, err := n.Int64(); err != nil {
			return ""
		}
		s = n.String()
	}
	if s == "" {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-') {
			return ""
		}
	}
	if len(s) > backendFieldMax {
		s = s[:backendFieldMax]
	}
	return s
}
