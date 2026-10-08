package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

// A 404 answer read for the deployment's failure: the readers of its error shapes,
// the missing-model rules the modules choose from, and the whole-word match.

// maxNotFoundBody is the most of a 404 answer read to tell the deployment's failure
// (a missing model, an unknown path) from a caller's 404; error answers are small.
const maxNotFoundBody = 64 << 10

// readNotFound reads the start of a 404 (or 405) answer, up to maxNotFoundBody bytes, and puts
// what was read back in front of the body, so an answer that is the caller's is
// relayed whole. ok is false when the read failed: such an answer is not the
// deployment's, and the body returns the error again after what was read.
func readNotFound(resp *http.Response) (answer []byte, ok bool) {
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxNotFoundBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return head, err == nil
}

// notFoundFields are the parts of a 404 answer a missing model is read from: its
// error message, code and type, wherever the server's shape puts them —
// {"error": {"message", "code", "type"}} (OpenAI, vLLM, and Anthropic's
// {"type": "error", "error": {...}}), {"message", "code"} at the top level (older
// vLLM), {"error": "<message>"} (Ollama). Fields that are not strings read as none.
type notFoundFields struct {
	message, code, errorType string
}

func readNotFoundFields(answer []byte) (f notFoundFields, ok bool) {
	var top struct {
		Error   json.RawMessage `json:"error"`
		Message json.RawMessage `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(answer, &top) != nil {
		return notFoundFields{}, false
	}
	message, code, errorType := top.Message, top.Code, json.RawMessage(nil)
	var nested struct {
		Message json.RawMessage `json:"message"`
		Code    json.RawMessage `json:"code"`
		Type    json.RawMessage `json:"type"`
	}
	switch {
	case json.Unmarshal(top.Error, new(string)) == nil:
		message = top.Error
	case json.Unmarshal(top.Error, &nested) == nil && len(top.Error) > 0:
		message, code, errorType = nested.Message, nested.Code, nested.Type
	}
	_ = json.Unmarshal(message, &f.message)
	_ = json.Unmarshal(code, &f.code)
	_ = json.Unmarshal(errorType, &f.errorType)
	return f, true
}

// missingModelNamedOrCoded is a self-hosted server's missing-model rule: the answer's
// message names the model as a whole word (vLLM: "The model `…` does not exist."),
// or its code is one of codes. These servers answer a request's own IDs only for
// what the gateway's clients cannot store there, so the name is a safe signal.
func missingModelNamedOrCoded(codes ...string) func(answer []byte, model string) bool {
	return func(answer []byte, model string) bool {
		f, ok := readNotFoundFields(answer)
		return ok && (slices.Contains(codes, f.code) || namesWord(f.message, model))
	}
}

// missingModelCoded is a cloud API's missing-model rule: the answer's code is one of
// codes. The message is not read: a request can make these APIs echo an ID it chose
// in a 404 (a Responses item_reference, a file_id), and a client naming the
// backend-side model there must not count as the deployment's failure
// (docs/specs/GATEWAY.md, Providers: wrong model on a host).
func missingModelCoded(codes ...string) func(answer []byte, model string) bool {
	return func(answer []byte, _ string) bool {
		f, ok := readNotFoundFields(answer)
		return ok && slices.Contains(codes, f.code)
	}
}

// errorAnswer is what the core reads of an error answer in the OpenAI format.
type errorAnswer struct {
	Type    string
	Message string
}

// readErrorAnswer reads an answer in the OpenAI error shape: {"error": {...}} (its
// type and message, where they are strings) or {"error": "<text>"} (the text as its
// message). ok is false for any other body — plain text, HTML, other JSON.
func readErrorAnswer(answer []byte) (e errorAnswer, ok bool) {
	var top struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(answer, &top) != nil {
		return errorAnswer{}, false
	}
	if json.Unmarshal(top.Error, &e.Message) == nil {
		return e, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(top.Error, &fields) != nil || fields == nil {
		return errorAnswer{}, false
	}
	_ = json.Unmarshal(fields["type"], &e.Type) // a type that is not a string reads as none
	_ = json.Unmarshal(fields["message"], &e.Message)
	return e, true
}

// namesWord reports whether text contains word with no model-name character right
// before or after it, so "llama" is not found in "llama-2".
func namesWord(text, word string) bool {
	if word == "" {
		return false
	}
	nameChar := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._/:-@+", c) >= 0
	}
	for from := 0; ; {
		i := strings.Index(text[from:], word)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(word)
		if (start == 0 || !nameChar(text[start-1])) && (end == len(text) || !nameChar(text[end])) {
			return true
		}
		from = start + 1
	}
}
