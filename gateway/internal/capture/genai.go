package capture

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"kaiak/internal/telemetry/otlptrace"
)

// The GenAI conventions' content format, which every captured API is mapped into
// (docs/specs/GATEWAY.md, Request capture → Content: one message format): messages of
// parts, tool definitions, and the span attributes that carry them as JSON strings
// (Named deviations: content as JSON strings). The shapes are the conventions' JSON
// schemas at the pinned commit (model/gen-ai/gen-ai-input-messages.json,
// …-output-messages.json, …-system-instructions.json, …-tool-definitions.json); the
// members of each type are written in the order the spec's examples show.

// message is one chat message: who wrote it, its parts and the name of its
// participant, "" for none.
type message struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
	Name  string `json:"name,omitempty"`
}

// newMessage is a message of role, by the participant name, made of parts — none
// written as an empty list: the conventions require the member.
func newMessage(role, name string, parts []part) message {
	if parts == nil {
		parts = []part{}
	}
	return message{Role: role, Parts: parts, Name: name}
}

// part is one of a message's parts — textPart (text and reasoning), toolCallPart,
// toolCallResponsePart, blobPart, uriPart, filePart — or original, a part the mapping
// does not know. The new…Part functions make each.
type part interface{ isPart() }

// textPart is a text or reasoning part.
type textPart struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

// toolCallPart is a call of a tool the client runs. Arguments is toolArguments'
// value: JSON, a string, or nil when the call has none.
type toolCallPart struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments any    `json:"arguments,omitempty"`
}

// toolCallResponsePart is a tool's result, as the client sent it: Response is null
// when it sent none.
type toolCallResponsePart struct {
	Type     string          `json:"type"`
	ID       string          `json:"id,omitempty"`
	Response json.RawMessage `json:"response"`
}

// blobPart is inline media: Content is its base64, as sent.
type blobPart struct {
	Type     string `json:"type"`
	Modality string `json:"modality"`
	MIMEType string `json:"mime_type,omitempty"`
	Content  string `json:"content"`
}

// uriPart is media by URL.
type uriPart struct {
	Type     string `json:"type"`
	Modality string `json:"modality"`
	MIMEType string `json:"mime_type,omitempty"`
	URI      string `json:"uri"`
}

// filePart is media by the ID of a file uploaded to the provider.
type filePart struct {
	Type     string `json:"type"`
	Modality string `json:"modality"`
	MIMEType string `json:"mime_type,omitempty"`
	FileID   string `json:"file_id"`
}

// original is a JSON object carried as it was sent — a part, block or item the
// mapping does not know (the conventions' generic part), or a tool that is not a
// function — which names its own type. As passthrough preserves what it does not
// understand, so does capture.
type original json.RawMessage

func (o original) MarshalJSON() ([]byte, error) { return o, nil }

func (textPart) isPart()             {}
func (toolCallPart) isPart()         {}
func (toolCallResponsePart) isPart() {}
func (blobPart) isPart()             {}
func (uriPart) isPart()              {}
func (filePart) isPart()             {}
func (original) isPart()             {}

func newTextPart(text string) part      { return textPart{Type: "text", Content: text} }
func newReasoningPart(text string) part { return textPart{Type: "reasoning", Content: text} }

func newToolCallPart(id, name string, arguments any) part {
	return toolCallPart{Type: "tool_call", ID: id, Name: name, Arguments: arguments}
}

func newToolCallResponsePart(id string, response json.RawMessage) part {
	return toolCallResponsePart{Type: "tool_call_response", ID: id, Response: response}
}

func newBlobPart(modality, mimeType, content string) part {
	return blobPart{Type: "blob", Modality: modality, MIMEType: mimeType, Content: content}
}

func newURIPart(modality, mimeType, uri string) part {
	return uriPart{Type: "uri", Modality: modality, MIMEType: mimeType, URI: uri}
}

func newFilePart(modality, mimeType, fileID string) part {
	return filePart{Type: "file", Modality: modality, MIMEType: mimeType, FileID: fileID}
}

// appendText appends text as a text part, and appendReasoning as a reasoning part —
// unless it is empty: an empty text carries nothing, and a stream's opening delta
// (content "") would otherwise make its answer differ from the same answer
// unstreamed (content null).
func appendText(parts []part, text string) []part {
	if text == "" {
		return parts
	}
	return append(parts, newTextPart(text))
}

func appendReasoning(parts []part, text string) []part {
	if text == "" {
		return parts
	}
	return append(parts, newReasoningPart(text))
}

// toolDefinition is one of the tools a request offers: functionTool, or original —
// any other tool, as the client sent it.
type toolDefinition interface{ isToolDefinition() }

// functionTool is a function the client runs; Parameters is its JSON schema, nil
// when the client gave none.
type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (functionTool) isToolDefinition() {}
func (original) isToolDefinition()     {}

func newFunctionTool(name, description string, parameters json.RawMessage) toolDefinition {
	return functionTool{Type: "function", Name: name, Description: description, Parameters: parameters}
}

// Media kinds — the conventions' modalities — as an API names them, and as a MIME type
// gives them.
const (
	modalityImage    = "image"
	modalityAudio    = "audio"
	modalityVideo    = "video"
	modalityDocument = "document"
)

// mimeType is s as the conventions carry a MIME type: its type and subtype,
// lowercase, without parameters; "" when s is not one.
func mimeType(s string) string {
	s, _, _ = strings.Cut(s, ";")
	s = strings.ToLower(strings.TrimSpace(s))
	if typ, subtype, ok := strings.Cut(s, "/"); !ok || typ == "" || subtype == "" {
		return ""
	}
	return s
}

// modality is the modality of media of MIME type mime (mimeType's form): image/*,
// audio/* and video/* their own, any other a document; with no MIME type, kind — the
// kind the API names.
func modality(mime, kind string) string {
	switch typ, _, _ := strings.Cut(mime, "/"); {
	case mime == "":
		return kind
	case typ == "image":
		return modalityImage
	case typ == "audio":
		return modalityAudio
	case typ == "video":
		return modalityVideo
	}
	return modalityDocument
}

// dataURL reads a base64 data: URL — its scheme and the base64 marker in any case,
// as URLs are read — into its MIME type (mimeType's form, "" when it names none) and
// its base64 content, as sent; ok is false for any other URL, a data: URL whose
// content is not base64 included.
func dataURL(url string) (mime, content string, ok bool) {
	const scheme = "data:"
	if len(url) < len(scheme) || !strings.EqualFold(url[:len(scheme)], scheme) {
		return "", "", false
	}
	header, content, found := strings.Cut(url[len(scheme):], ",")
	const marker = ";base64"
	if !found || len(header) < len(marker) || !strings.EqualFold(header[len(header)-len(marker):], marker) {
		return "", "", false
	}
	return mimeType(header[:len(header)-len(marker)]), content, true
}

// newMediaByURL is media given by URL: a blob for a base64 data: URL, its modality
// from its MIME type, else kind; a uri part, of modality kind, for any other URL.
func newMediaByURL(url, kind string) part {
	if mime, content, ok := dataURL(url); ok {
		return newBlobPart(modality(mime, kind), mime, content)
	}
	return newURIPart(kind, "", url)
}

// toolArguments is a tool call's arguments as the conventions carry them, from raw,
// the call's member as sent: a string is read by stringArguments; any other value is
// itself, as JSON; nil for none — absent or null.
func toolArguments(raw json.RawMessage) any {
	if isAbsent(raw) {
		return nil
	}
	if s, ok := stringOf(raw); ok {
		return stringArguments(s)
	}
	return raw
}

// stringArguments is a tool call's arguments given as a string: the JSON it holds
// when it is valid JSON, else the string itself — a stream cut in the middle of a call
// leaves its arguments half-written.
func stringArguments(s string) any {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return s
}

// requestContent is what a request gives its capture: its parameters, and its
// content in the conventions' format.
type requestContent struct {
	parameters         parameters
	systemInstructions []part
	messages           []message
	tools              []toolDefinition
}

// answerContent is what an answer gives its capture: its ID and model, "" when it
// names none, its output messages — one per generation — and their finish reasons,
// one per message, "" for one that gave none.
type answerContent struct {
	id, model     string
	messages      []message
	finishReasons []string
}

// contentAttributes are a capture's content attributes, from what its request and
// its answer give (docs/specs/GATEWAY.md, Request capture → Attributes): each only
// when there is something to carry. The finish reasons are written when any
// generation gave one.
func contentAttributes(rq requestContent, an answerContent) []otlptrace.Attribute {
	attrs := []otlptrace.Attribute(rq.parameters)
	if an.id != "" {
		attrs = append(attrs, otlptrace.String("gen_ai.response.id", an.id))
	}
	if an.model != "" {
		attrs = append(attrs, otlptrace.String("gen_ai.response.model", an.model))
	}
	if slices.ContainsFunc(an.finishReasons, func(r string) bool { return r != "" }) {
		attrs = append(attrs, otlptrace.Strings("gen_ai.response.finish_reasons", an.finishReasons))
	}
	if len(rq.systemInstructions) > 0 {
		attrs = appendJSON(attrs, "gen_ai.system_instructions", rq.systemInstructions)
	}
	if len(rq.messages) > 0 {
		attrs = appendJSON(attrs, "gen_ai.input.messages", rq.messages)
	}
	if len(rq.tools) > 0 {
		attrs = appendJSON(attrs, "gen_ai.tool.definitions", rq.tools)
	}
	if len(an.messages) > 0 {
		attrs = appendJSON(attrs, "gen_ai.output.messages", an.messages)
	}
	return attrs
}

// appendJSON appends v, as a JSON string, as attribute key. The JSON is written
// without HTML escaping: a prompt's every <, > and & would otherwise become a
// six-byte escape — larger, escaped again inside the span, and not what the client
// sent. Content built from parsed JSON always encodes; should it not, the attribute
// is left out rather than written wrong.
func appendJSON(attrs []otlptrace.Attribute, key string, v any) []otlptrace.Attribute {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return attrs
	}
	return append(attrs, otlptrace.String(key, string(bytes.TrimSuffix(b.Bytes(), []byte{'\n'}))))
}
