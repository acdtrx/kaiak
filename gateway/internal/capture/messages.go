package capture

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// messagesMapping reads a Messages exchange into the conventions' format
// (docs/specs/GATEWAY.md, Request capture → Messages; Streams).
var messagesMapping = mapping{request: messagesRequest, answer: messagesAnswer, stream: messagesStream}

// messagesRequest is a Messages request's parameters, system instructions, messages
// and tools.
func messagesRequest(body []byte) requestContent {
	top, ok := members(body)
	if !ok {
		return requestContent{}
	}
	var rc requestContent
	p := &rc.parameters
	p.addInteger("gen_ai.request.max_tokens", top["max_tokens"])
	p.addNumber("gen_ai.request.temperature", top["temperature"])
	p.addNumber("gen_ai.request.top_p", top["top_p"])
	p.addInteger("gen_ai.request.top_k", top["top_k"])
	p.addStrings("gen_ai.request.stop_sequences", top["stop_sequences"])
	rc.systemInstructions = messagesSystem(top["system"])
	list, _ := elements(top["messages"])
	for _, raw := range list {
		if m, ok := messagesInputMessage(raw); ok {
			rc.messages = append(rc.messages, m)
		}
	}
	tools, _ := elements(top["tools"])
	for _, raw := range tools {
		if t, ok := messagesTool(raw); ok {
			rc.tools = append(rc.tools, t)
		}
	}
	return rc
}

// messagesSystem is a request's system prompt as system instructions: a string is
// one text part, a list its text blocks as text parts and any other block as sent
// (appendGeneric). A block that is not an object naming its type, or a text block
// without its text, is left out.
func messagesSystem(raw json.RawMessage) []part {
	if s, ok := stringOf(raw); ok {
		return appendText(nil, s)
	}
	var parts []part
	blocks, _ := elements(raw)
	for _, block := range blocks {
		m, ok := members(block)
		if !ok {
			continue
		}
		switch typ, _ := stringOf(m["type"]); typ {
		case "":
		case "text":
			s, _ := stringOf(m["text"])
			parts = appendText(parts, s)
		default:
			parts = appendGeneric(parts, block, typ)
		}
	}
	return parts
}

// messagesInputMessage is one of a request's messages, with its role: a string
// content is one text part, a list its blocks. false for a message that is not an
// object with a role.
func messagesInputMessage(raw json.RawMessage) (message, bool) {
	m, ok := members(raw)
	if !ok {
		return message{}, false
	}
	role, ok := stringOf(m["role"])
	if !ok {
		return message{}, false
	}
	if s, ok := stringOf(m["content"]); ok {
		return newMessage(role, "", appendText(nil, s)), true
	}
	var parts []part
	blocks, _ := elements(m["content"])
	for _, block := range blocks {
		parts = appendMessagesBlock(parts, block)
	}
	return newMessage(role, "", parts), true
}

// appendMessagesBlock appends one content block: text as text; thinking as
// reasoning, its signature left out; image and document by their source
// (appendMessagesMedia); tool_use as a tool call — its id, name and input;
// tool_result as a tool result — its tool_use_id and its content as sent, a string
// or its blocks' JSON; any other type (redacted_thinking, a server tool's blocks, …)
// as sent (appendGeneric). A block that is not an object naming its type, or a known
// block without its API's shape, is left out.
func appendMessagesBlock(parts []part, raw json.RawMessage) []part {
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
	case "thinking":
		if s, ok := stringOf(m["thinking"]); ok {
			return appendReasoning(parts, s)
		}
	case "image":
		return appendMessagesMedia(parts, raw, m["source"], modalityImage)
	case "document":
		return appendMessagesMedia(parts, raw, m["source"], modalityDocument)
	case "tool_use":
		if name, ok := stringOf(m["name"]); ok {
			id, _ := stringOf(m["id"])
			return append(parts, newToolCallPart(id, name, toolArguments(m["input"])))
		}
	case "tool_result":
		id, _ := stringOf(m["tool_use_id"])
		return append(parts, newToolCallResponsePart(id, m["content"]))
	default:
		return appendGeneric(parts, raw, typ)
	}
	return parts
}

// appendMessagesMedia appends block, an image or a document — media of kind — by its
// source: a base64 source as a blob, its media_type and its data; a url source as a
// uri; a source of any other type (text, content) as the block as sent. A source
// that is not an object naming its type, or a base64 or url source without its
// shape, is left out. A file source never reaches capture: the inbound stage refuses
// a stored object.
func appendMessagesMedia(parts []part, block, rawSource json.RawMessage, kind string) []part {
	source, _ := members(rawSource)
	switch typ, _ := stringOf(source["type"]); typ {
	case "":
		return parts
	case "base64":
		data, ok := stringOf(source["data"])
		if !ok {
			return parts
		}
		mediaType, _ := stringOf(source["media_type"])
		mime := mimeType(mediaType)
		return append(parts, newBlobPart(modality(mime, kind), mime, data))
	case "url":
		url, ok := stringOf(source["url"])
		if !ok {
			return parts
		}
		return append(parts, newURIPart(kind, "", url))
	}
	return append(parts, original(block))
}

// messagesTool is one of the tools a request offers: a tool with an input_schema —
// a custom tool — as a function definition, its input_schema as the parameters; any
// other (Anthropic's client-run tools, bash_20250124, …) as sent, named as
// genericTool names it. false for a tool that is neither, or a custom tool without a
// name.
func messagesTool(raw json.RawMessage) (toolDefinition, bool) {
	m, ok := members(raw)
	if !ok {
		return nil, false
	}
	if !isAbsent(m["input_schema"]) {
		return readFunctionTool(m, "input_schema")
	}
	if typ, _ := stringOf(m["type"]); typ != "" {
		return genericTool(raw, m, "")
	}
	return nil, false
}

// messagesAnswer is a Messages answer's ID, model and its one output message — its
// content blocks, role assistant — with its stop_reason as the finish reason. An
// answer whose content is not a list has no output message.
func messagesAnswer(body []byte) answerContent {
	top, ok := members(body)
	if !ok {
		return answerContent{}
	}
	var an answerContent
	an.id, _ = stringOf(top["id"])
	an.model, _ = stringOf(top["model"])
	blocks, ok := elements(top["content"])
	if !ok {
		return an
	}
	var parts []part
	for _, block := range blocks {
		parts = appendMessagesBlock(parts, block)
	}
	stopReason, _ := stringOf(top["stop_reason"])
	an.messages = []message{newMessage("assistant", "", parts)}
	an.finishReasons = []string{stopReason}
	return an
}

// messagesStream is a streamed Messages answer assembled into what the same answer
// unstreamed gives: message_start opens the message — its ID and model; content
// blocks are gathered by index, as they may interleave (llama-server opens one
// before closing the previous); message_delta gives the stop reason; message_stop
// ends it. A stream cut short gives what arrived; events before message_start are
// not read.
func messagesStream(stream []byte) answerContent {
	var an answerContent
	started := false
	var stopReason string
	blocks := map[int64]*messagesBlock{}
events:
	for data := range streamEvents(stream) {
		event, ok := members(data)
		if !ok {
			continue
		}
		typ, _ := stringOf(event["type"])
		if !started {
			if typ == "message_start" {
				started = true
				m, _ := members(event["message"])
				an.id, _ = stringOf(m["id"])
				an.model, _ = stringOf(m["model"])
			}
			continue
		}
		switch typ {
		case "message_stop":
			break events
		case "message_delta":
			delta, _ := members(event["delta"])
			if reason, _ := stringOf(delta["stop_reason"]); reason != "" {
				stopReason = reason
			}
		case "content_block_start", "content_block_delta", "content_block_stop":
			index, ok := integerOf(event["index"])
			if !ok {
				continue
			}
			// A block opens once; one never opened, or stopped, takes nothing more.
			b := blocks[index]
			switch {
			case typ == "content_block_start" && b == nil:
				if b, ok := startMessagesBlock(event["content_block"]); ok {
					blocks[index] = b
				}
			case b == nil || b.stopped:
			case typ == "content_block_delta":
				delta, _ := members(event["delta"])
				b.add(delta)
			case typ == "content_block_stop":
				b.stopped = true
			}
		}
	}
	if !started {
		return answerContent{}
	}
	var parts []part
	for _, index := range slices.Sorted(maps.Keys(blocks)) {
		parts = blocks[index].appendTo(parts)
	}
	an.messages = []message{newMessage("assistant", "", parts)}
	an.finishReasons = []string{stopReason}
	return an
}

// messagesBlock is one content block of a streamed answer, as its start and its
// deltas have built it so far.
type messagesBlock struct {
	// typ is its type: text, thinking and tool_use are built from deltas; a block of
	// any other type is carried as its start gave it (start).
	typ   string
	start json.RawMessage
	// id, name and input are a tool_use block's from its start.
	id, name string
	hasName  bool
	input    json.RawMessage
	// content is the text, the thinking, or the tool input's JSON fragments.
	content strings.Builder
	// stopped: content_block_stop closed it; a later delta is not read.
	stopped bool
}

// startMessagesBlock is the block a content_block_start opens, from its content
// block: text and thinking with the text they start with; tool_use with its id, name
// and input; any other type as sent. false for a block that is not an object naming
// its type.
func startMessagesBlock(raw json.RawMessage) (*messagesBlock, bool) {
	m, ok := members(raw)
	if !ok {
		return nil, false
	}
	typ, _ := stringOf(m["type"])
	if typ == "" {
		return nil, false
	}
	b := &messagesBlock{typ: typ, start: raw}
	switch typ {
	case "text":
		s, _ := stringOf(m["text"])
		b.content.WriteString(s)
	case "thinking":
		s, _ := stringOf(m["thinking"])
		b.content.WriteString(s)
	case "tool_use":
		b.id, _ = stringOf(m["id"])
		b.name, b.hasName = stringOf(m["name"])
		b.input = m["input"]
	}
	return b, true
}

// add adds a content_block_delta's delta to the block of its kind: text_delta to
// text, thinking_delta to thinking, input_json_delta's fragment to tool_use. A
// signature_delta, and a delta of any other kind or to a block of another kind, are
// not read.
func (b *messagesBlock) add(delta map[string]json.RawMessage) {
	member := ""
	switch kind, _ := stringOf(delta["type"]); {
	case kind == "text_delta" && b.typ == "text":
		member = "text"
	case kind == "thinking_delta" && b.typ == "thinking":
		member = "thinking"
	case kind == "input_json_delta" && b.typ == "tool_use":
		member = "partial_json"
	default:
		return
	}
	s, _ := stringOf(delta[member])
	b.content.WriteString(s)
}

// appendTo appends the block's part, as its unstreamed block gives it. A tool call's
// input is its fragments joined — a JSON object when whole, the string when cut
// short — or, with no fragment, the input its start gave.
func (b *messagesBlock) appendTo(parts []part) []part {
	switch b.typ {
	case "text":
		return appendText(parts, b.content.String())
	case "thinking":
		return appendReasoning(parts, b.content.String())
	case "tool_use":
		if !b.hasName {
			return parts
		}
		arguments := toolArguments(b.input)
		if b.content.Len() > 0 {
			arguments = stringArguments(b.content.String())
		}
		return append(parts, newToolCallPart(b.id, b.name, arguments))
	}
	return appendGeneric(parts, b.start, b.typ)
}
