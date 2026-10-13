package capture

import (
	"bytes"
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"kaiak/internal/telemetry/otlptrace"
)

// chatCompletionsMapping reads a Chat Completions exchange into the conventions'
// format (docs/specs/GATEWAY.md, Request capture → Chat Completions; Streams).
var chatCompletionsMapping = mapping{request: chatCompletionsRequest, answer: chatCompletionsAnswer,
	stream: chatCompletionsStream}

// chatCompletionsRequest is a Chat Completions request's parameters, messages and
// tools.
func chatCompletionsRequest(body []byte) requestContent {
	top, ok := members(body)
	if !ok {
		return requestContent{}
	}
	var rc requestContent
	p := &rc.parameters
	p.addInteger("gen_ai.request.max_tokens", top["max_completion_tokens"], top["max_tokens"])
	p.addNumber("gen_ai.request.temperature", top["temperature"])
	p.addNumber("gen_ai.request.top_p", top["top_p"])
	p.addInteger("gen_ai.request.top_k", top["top_k"])
	p.addNumber("gen_ai.request.frequency_penalty", top["frequency_penalty"])
	p.addNumber("gen_ai.request.presence_penalty", top["presence_penalty"])
	p.addStrings("gen_ai.request.stop_sequences", top["stop"])
	p.addInteger("gen_ai.request.seed", top["seed"])
	// The convention sets the choice count only when it is not 1.
	if n, ok := integerOf(top["n"]); ok && n != 1 {
		*p = append(*p, otlptrace.Int("gen_ai.request.choice.count", n))
	}
	list, _ := elements(top["messages"])
	for _, raw := range list {
		if m, ok := chatInputMessage(raw); ok {
			rc.messages = append(rc.messages, m)
		}
	}
	tools, _ := elements(top["tools"])
	for _, raw := range tools {
		if t, ok := chatTool(raw); ok {
			rc.tools = append(rc.tools, t)
		}
	}
	return rc
}

// chatInputMessage is one of a request's messages, with its role — system and
// developer included: they are chat history — and its name. A tool message is the
// result of a call: its content as sent, a string or its JSON. false for a message
// that is not an object with a role.
func chatInputMessage(raw json.RawMessage) (message, bool) {
	m, ok := members(raw)
	if !ok {
		return message{}, false
	}
	role, ok := stringOf(m["role"])
	if !ok {
		return message{}, false
	}
	name, _ := stringOf(m["name"])
	if role == "tool" {
		id, _ := stringOf(m["tool_call_id"])
		return newMessage(role, name, []part{newToolCallResponsePart(id, m["content"])}), true
	}
	return newMessage(role, name, chatMessageParts(m)), true
}

// chatMessageParts are the parts of a message — of a request, or of a choice of the
// answer — in this order: its reasoning, its content (a string, or a list of parts),
// its refusal (model text), its tool calls.
func chatMessageParts(m map[string]json.RawMessage) []part {
	parts := appendReasoning(nil, chatReasoning(m))
	if s, ok := stringOf(m["content"]); ok {
		parts = appendText(parts, s)
	} else if l, ok := elements(m["content"]); ok {
		for _, raw := range l {
			parts = appendChatContentPart(parts, raw)
		}
	}
	refusal, _ := stringOf(m["refusal"])
	parts = appendText(parts, refusal)
	calls, _ := elements(m["tool_calls"])
	for _, raw := range calls {
		parts = appendChatToolCall(parts, raw)
	}
	return parts
}

// chatReasoning is the reasoning a message or a stream's delta carries:
// reasoning_content (llama-server's) when set, else reasoning (vLLM's) — a server may
// send the same text under both names.
func chatReasoning(m map[string]json.RawMessage) string {
	if s, _ := stringOf(m["reasoning_content"]); s != "" {
		return s
	}
	s, _ := stringOf(m["reasoning"])
	return s
}

// appendChatContentPart appends one part of a message's content: text and refusal
// as text; image_url as a blob (a base64 data: URL) or uri, modality image;
// input_audio as a blob, modality audio, MIME type audio/<format>; file as a file
// (file_id) or blob (file_data, a base64 data: URL); any other type as sent. A part
// that is not an object naming its type, or a known part without its API's shape, is
// left out.
func appendChatContentPart(parts []part, raw json.RawMessage) []part {
	m, ok := members(raw)
	if !ok {
		return parts
	}
	typ, _ := stringOf(m["type"])
	switch typ {
	case "":
		return parts
	case "text":
		if s, ok := stringOf(m["text"]); ok {
			return appendText(parts, s)
		}
	case "refusal":
		if s, ok := stringOf(m["refusal"]); ok {
			return appendText(parts, s)
		}
	case "image_url":
		image, _ := members(m["image_url"])
		if url, ok := stringOf(image["url"]); ok {
			return append(parts, newMediaByURL(url, modalityImage))
		}
	case "input_audio":
		audio, _ := members(m["input_audio"])
		if data, ok := stringOf(audio["data"]); ok {
			mime := ""
			if format, _ := stringOf(audio["format"]); format != "" {
				mime = mimeType("audio/" + format)
			}
			return append(parts, newBlobPart(modality(mime, modalityAudio), mime, data))
		}
	case "file":
		file, _ := members(m["file"])
		if id, ok := stringOf(file["file_id"]); ok {
			return append(parts, newFilePart(modalityDocument, "", id))
		}
		data, _ := stringOf(file["file_data"])
		if mime, content, ok := dataURL(data); ok {
			return append(parts, newBlobPart(modality(mime, modalityDocument), mime, content))
		}
	default:
		return append(parts, original(raw))
	}
	return parts
}

// appendChatToolCall appends one of a message's tool calls: a function's as a tool
// call — its id, function.name and function.arguments — and a call of any other type
// as sent. A call that is neither is left out.
func appendChatToolCall(parts []part, raw json.RawMessage) []part {
	m, ok := members(raw)
	if !ok {
		return parts
	}
	if fn, ok := members(m["function"]); ok {
		name, ok := stringOf(fn["name"])
		if !ok {
			return parts
		}
		id, _ := stringOf(m["id"])
		return append(parts, newToolCallPart(id, name, toolArguments(fn["arguments"])))
	}
	if typ, _ := stringOf(m["type"]); typ != "" {
		return append(parts, original(raw))
	}
	return parts
}

// chatTool is one of the tools a request offers: a function tool as a function
// definition — its name, description and parameters — and any other as sent. false
// for a tool that is not an object naming its type, or a function without a name.
func chatTool(raw json.RawMessage) (toolDefinition, bool) {
	m, ok := members(raw)
	if !ok {
		return nil, false
	}
	switch typ, _ := stringOf(m["type"]); typ {
	case "":
		return nil, false
	case "function":
		fn, _ := members(m["function"])
		name, ok := stringOf(fn["name"])
		if !ok {
			return nil, false
		}
		description, _ := stringOf(fn["description"])
		var parameters json.RawMessage
		if !isAbsent(fn["parameters"]) {
			parameters = fn["parameters"]
		}
		return newFunctionTool(name, description, parameters), true
	}
	return original(raw), true
}

// chatOutputRole is an output message's role: the one the choice names, assistant
// when it names none.
func chatOutputRole(role string) string { return cmp.Or(role, "assistant") }

// chatCompletionsAnswer is a Chat Completions answer's ID, model and choices — each
// one output message, in index order, with its finish reason. A choice without an
// index or a message is left out.
func chatCompletionsAnswer(body []byte) answerContent {
	top, ok := members(body)
	if !ok {
		return answerContent{}
	}
	var an answerContent
	an.id, _ = stringOf(top["id"])
	an.model, _ = stringOf(top["model"])
	type choice struct {
		index        int64
		message      message
		finishReason string
	}
	var choices []choice
	list, _ := elements(top["choices"])
	for _, raw := range list {
		c, _ := members(raw)
		index, ok := integerOf(c["index"])
		if !ok {
			continue
		}
		m, ok := members(c["message"])
		if !ok {
			continue
		}
		role, _ := stringOf(m["role"])
		finishReason, _ := stringOf(c["finish_reason"])
		choices = append(choices, choice{index: index, message: newMessage(chatOutputRole(role), "", chatMessageParts(m)),
			finishReason: finishReason})
	}
	slices.SortStableFunc(choices, func(a, b choice) int { return cmp.Compare(a.index, b.index) })
	for _, c := range choices {
		an.messages = append(an.messages, c.message)
		an.finishReasons = append(an.finishReasons, c.finishReason)
	}
	return an
}

// chatCompletionsStream is a streamed Chat Completions answer assembled into what
// the same answer unstreamed gives: its chunks gathered by choice index, each
// choice's deltas concatenated, its tool calls by their own index; the chunks' ID and
// model. [DONE] ends it, and a stream cut short gives what arrived.
func chatCompletionsStream(stream []byte) answerContent {
	var an answerContent
	choices := map[int64]*chatChoice{}
	for data := range streamEvents(stream) {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			break
		}
		chunk, ok := members(data)
		if !ok {
			continue
		}
		if an.id == "" {
			an.id, _ = stringOf(chunk["id"])
		}
		if an.model == "" {
			an.model, _ = stringOf(chunk["model"])
		}
		// A chunk without choices — the usage chunk — carries no content.
		list, _ := elements(chunk["choices"])
		for _, raw := range list {
			c, _ := members(raw)
			index, ok := integerOf(c["index"])
			if !ok {
				continue
			}
			if choices[index] == nil {
				choices[index] = &chatChoice{}
			}
			choices[index].add(c)
		}
	}
	for _, index := range slices.Sorted(maps.Keys(choices)) {
		c := choices[index]
		an.messages = append(an.messages, c.message())
		an.finishReasons = append(an.finishReasons, c.finishReason)
	}
	return an
}

// chatChoice is one choice of a streamed answer, as its chunks have built it so far.
type chatChoice struct {
	role, finishReason          string
	reasoning, content, refusal strings.Builder
	toolCalls                   map[int64]*chatToolCall
}

// chatToolCall is one tool call of a streamed choice, as its fragments have built it
// so far. Only a function's call can be rebuilt from its fragments: a call no
// fragment gave a function is left out.
type chatToolCall struct {
	id, name               string
	function, hasArguments bool
	arguments              strings.Builder
}

// add adds one chunk's choice c: its finish reason, and its delta — the role and the
// ID and name of each tool call from the fragments that first carry them; the
// reasoning, content, refusal and each call's arguments concatenated.
func (c *chatChoice) add(choice map[string]json.RawMessage) {
	if reason, _ := stringOf(choice["finish_reason"]); reason != "" {
		c.finishReason = reason
	}
	delta, ok := members(choice["delta"])
	if !ok {
		return
	}
	if role, _ := stringOf(delta["role"]); c.role == "" {
		c.role = role
	}
	c.reasoning.WriteString(chatReasoning(delta))
	content, _ := stringOf(delta["content"])
	c.content.WriteString(content)
	refusal, _ := stringOf(delta["refusal"])
	c.refusal.WriteString(refusal)
	fragments, _ := elements(delta["tool_calls"])
	for _, raw := range fragments {
		f, _ := members(raw)
		index, ok := integerOf(f["index"])
		if !ok {
			continue
		}
		if c.toolCalls == nil {
			c.toolCalls = map[int64]*chatToolCall{}
		}
		call := c.toolCalls[index]
		if call == nil {
			call = &chatToolCall{}
			c.toolCalls[index] = call
		}
		if id, _ := stringOf(f["id"]); call.id == "" {
			call.id = id
		}
		fn, ok := members(f["function"])
		if !ok {
			continue
		}
		call.function = true
		if name, _ := stringOf(fn["name"]); call.name == "" {
			call.name = name
		}
		if arguments, ok := stringOf(fn["arguments"]); ok {
			call.arguments.WriteString(arguments)
			call.hasArguments = true
		}
	}
}

// message is the choice's output message, its parts in the order an unstreamed
// choice's are.
func (c *chatChoice) message() message {
	parts := appendReasoning(nil, c.reasoning.String())
	parts = appendText(parts, c.content.String())
	parts = appendText(parts, c.refusal.String())
	for _, index := range slices.Sorted(maps.Keys(c.toolCalls)) {
		call := c.toolCalls[index]
		if !call.function {
			continue
		}
		var arguments any
		if call.hasArguments {
			arguments = stringArguments(call.arguments.String())
		}
		parts = append(parts, newToolCallPart(call.id, call.name, arguments))
	}
	return newMessage(chatOutputRole(c.role), "", parts)
}
