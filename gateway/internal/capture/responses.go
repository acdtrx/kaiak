package capture

import (
	"encoding/json"
	"slices"
	"strings"
)

// responsesMapping reads a Responses exchange into the conventions' format
// (docs/specs/GATEWAY.md, Request capture → Responses; Streams).
var responsesMapping = mapping{request: responsesRequest, answer: responsesAnswer, stream: responsesStream}

// responsesRequest is a Responses request's parameters, instructions, input and
// tools.
func responsesRequest(body []byte) requestContent {
	top, ok := members(body)
	if !ok {
		return requestContent{}
	}
	var rc requestContent
	p := &rc.parameters
	p.addInteger("gen_ai.request.max_tokens", top["max_output_tokens"])
	p.addNumber("gen_ai.request.temperature", top["temperature"])
	p.addNumber("gen_ai.request.top_p", top["top_p"])
	if s, ok := stringOf(top["instructions"]); ok {
		rc.systemInstructions = appendText(nil, s)
	}
	if s, ok := stringOf(top["input"]); ok {
		rc.messages = []message{newMessage("user", "", appendText(nil, s))}
	}
	items, _ := elements(top["input"])
	for _, raw := range items {
		if m, ok := responsesInputItem(raw); ok {
			rc.messages = append(rc.messages, m)
		}
	}
	tools, _ := elements(top["tools"])
	for _, raw := range tools {
		if t, ok := responsesTool(raw); ok {
			rc.tools = append(rc.tools, t)
		}
	}
	return rc
}

// responsesToolOutputs are the input items holding the output of a tool the client
// ran other than a function: each is a tool message.
var responsesToolOutputs = []string{"custom_tool_call_output", "local_shell_call_output", "shell_call_output",
	"apply_patch_call_output"}

// responsesInputItem is one input item as one message: a message item (type message,
// or a role and no type) with its role and content; a function_call as an assistant
// message holding the tool call and a function_call_output as a tool message holding
// its result — so that a tool round trip reads as on Chat Completions; a reasoning
// item as an assistant message of its reasoning; any other item as sent
// (appendGeneric), in a tool message for the output of a tool the client ran, else an
// assistant message. false for an item that is not an object naming its type or
// role, a known item without its API's shape, or an item appendGeneric leaves out.
func responsesInputItem(raw json.RawMessage) (message, bool) {
	m, ok := members(raw)
	if !ok {
		return message{}, false
	}
	if isAbsent(m["type"]) {
		return responsesMessageItem(m)
	}
	typ, _ := stringOf(m["type"])
	switch typ {
	case "":
		return message{}, false
	case "message":
		return responsesMessageItem(m)
	case "function_call":
		call, ok := responsesFunctionCall(m)
		if !ok {
			return message{}, false
		}
		return newMessage("assistant", "", []part{call}), true
	case "function_call_output":
		id, _ := stringOf(m["call_id"])
		return newMessage("tool", "", []part{newToolCallResponsePart(id, m["output"])}), true
	case "reasoning":
		return newMessage("assistant", "", appendResponsesReasoning(nil, m)), true
	}
	parts := appendGeneric(nil, raw, typ)
	if len(parts) == 0 {
		return message{}, false
	}
	role := "assistant"
	if slices.Contains(responsesToolOutputs, typ) {
		role = "tool"
	}
	return newMessage(role, "", parts), true
}

// responsesMessageItem is a message item with its role and its content's parts.
// false for an item without a role.
func responsesMessageItem(item map[string]json.RawMessage) (message, bool) {
	role, ok := stringOf(item["role"])
	if !ok {
		return message{}, false
	}
	return newMessage(role, "", appendResponsesContent(nil, item["content"])), true
}

// appendResponsesContent appends a message item's content: a string as one text
// part, a list as its parts.
func appendResponsesContent(parts []part, raw json.RawMessage) []part {
	if s, ok := stringOf(raw); ok {
		return appendText(parts, s)
	}
	list, _ := elements(raw)
	for _, p := range list {
		parts = appendResponsesContentPart(parts, p)
	}
	return parts
}

// appendResponsesContentPart appends one part of a message item's content:
// input_text and output_text as text, refusal as text; input_image as a blob (a
// base64 data: URL in image_url) or a uri (another URL), modality image; input_file
// as a blob (file_data, a base64 data: URL) or a uri (file_url), modality document
// unless a MIME type says otherwise; any other type as sent (appendGeneric). A part
// that is not an object naming its type, or a known part without its API's shape, is
// left out. A part naming a file_id never reaches capture: the inbound stage refuses
// a stored object.
func appendResponsesContentPart(parts []part, raw json.RawMessage) []part {
	m, ok := members(raw)
	if !ok {
		return parts
	}
	switch typ, _ := stringOf(m["type"]); typ {
	case "":
	case "input_text", "output_text":
		if s, ok := stringOf(m["text"]); ok {
			return appendText(parts, s)
		}
	case "refusal":
		if s, ok := stringOf(m["refusal"]); ok {
			return appendText(parts, s)
		}
	case "input_image":
		if url, ok := stringOf(m["image_url"]); ok {
			return append(parts, newMediaByURL(url, modalityImage))
		}
	case "input_file":
		if data, ok := stringOf(m["file_data"]); ok {
			if mime, content, ok := dataURL(data); ok {
				return append(parts, newBlobPart(modality(mime, modalityDocument), mime, content))
			}
			return parts
		}
		if url, ok := stringOf(m["file_url"]); ok {
			return append(parts, newMediaByURL(url, modalityDocument))
		}
	default:
		return appendGeneric(parts, raw, typ)
	}
	return parts
}

// responsesFunctionCall is a function_call item as a tool call: its call_id, name and
// arguments. false for a call without a name.
func responsesFunctionCall(item map[string]json.RawMessage) (part, bool) {
	name, ok := stringOf(item["name"])
	if !ok {
		return nil, false
	}
	id, _ := stringOf(item["call_id"])
	return newToolCallPart(id, name, toolArguments(item["arguments"])), true
}

// appendResponsesReasoning appends a reasoning item's texts as reasoning parts: each
// summary_text of its summary, then each reasoning_text of its content (vLLM's and
// llama-server's reasoning) — an entry of any other type as sent (appendGeneric). Its
// encrypted content is not carried. An entry that is not an object naming its type,
// or a known entry without its text, is left out.
func appendResponsesReasoning(parts []part, item map[string]json.RawMessage) []part {
	for _, list := range []struct{ member, kind string }{{"summary", "summary_text"}, {"content", "reasoning_text"}} {
		entries, _ := elements(item[list.member])
		for _, raw := range entries {
			m, ok := members(raw)
			if !ok {
				continue
			}
			switch typ, _ := stringOf(m["type"]); typ {
			case "":
			case list.kind:
				s, _ := stringOf(m["text"])
				parts = appendReasoning(parts, s)
			default:
				parts = appendGeneric(parts, raw, typ)
			}
		}
	}
	return parts
}

// responsesTool is one of the tools a request offers: a function tool as a function
// definition — its name, description and parameters — and any other as sent, named
// as genericTool names it (local_shell, apply_patch, … have no name). false for a
// tool that is not an object naming its type, or a function without a name.
func responsesTool(raw json.RawMessage) (toolDefinition, bool) {
	m, ok := members(raw)
	if !ok {
		return nil, false
	}
	switch typ, _ := stringOf(m["type"]); typ {
	case "":
		return nil, false
	case "function":
		return readFunctionTool(m, "parameters")
	}
	return genericTool(raw, m, "")
}

// responsesAnswer is a Responses answer — a response object — as
// responseObjectContent reads it.
func responsesAnswer(body []byte) answerContent {
	response, ok := members(body)
	if !ok {
		return answerContent{}
	}
	return responseObjectContent(response)
}

// responseObjectContent is a response object's ID, model and its one output message
// — its output items in order, role assistant — with its finish reason: the
// incomplete_details.reason of an incomplete response that has one, unfinished for a
// failed one, else its status. A response whose output is not a list has no output
// message.
func responseObjectContent(response map[string]json.RawMessage) answerContent {
	var an answerContent
	an.id, _ = stringOf(response["id"])
	an.model, _ = stringOf(response["model"])
	items, ok := elements(response["output"])
	if !ok {
		return an
	}
	var parts []part
	for _, raw := range items {
		parts = appendResponsesOutputItem(parts, raw)
	}
	status, _ := stringOf(response["status"])
	reason := status
	switch status {
	case "incomplete":
		details, _ := members(response["incomplete_details"])
		if s, _ := stringOf(details["reason"]); s != "" {
			reason = s
		}
	case "failed":
		reason = unfinished
	}
	an.messages = []message{newMessage("assistant", "", parts)}
	an.finishReasons = []string{reason}
	return an
}

// appendResponsesOutputItem appends an output item's parts: a message item's content
// (output_text and refusal as text), a function_call as a tool call, a reasoning
// item's texts as reasoning, any other item as sent (appendGeneric). An item that is
// not an object naming its type, or a function call without a name, is left out.
func appendResponsesOutputItem(parts []part, raw json.RawMessage) []part {
	m, ok := members(raw)
	if !ok {
		return parts
	}
	switch typ, _ := stringOf(m["type"]); typ {
	case "":
		return parts
	case "message":
		return appendResponsesContent(parts, m["content"])
	case "function_call":
		if call, ok := responsesFunctionCall(m); ok {
			return append(parts, call)
		}
		return parts
	case "reasoning":
		return appendResponsesReasoning(parts, m)
	default:
		return appendGeneric(parts, raw, typ)
	}
}

// responsesTerminalEvents end a Responses stream, each carrying the whole response.
var responsesTerminalEvents = []string{"response.completed", "response.incomplete", "response.failed"}

// responsesStream is a streamed Responses answer: the response object of its
// terminal event, read as an unstreamed answer. A stream without one — cut short, or
// ended without it — gives what arrived: the ID and model of response.created or
// response.in_progress, and the output items in the order they were added, each as
// response.output_item.done finished it or, still in progress, as its deltas have
// built it (responsesItem). Deltas name their item by item_id: llama-server's carry
// no output_index, and its items interleave. With no terminal event its one
// generation gave no finish reason.
func responsesStream(stream []byte) answerContent {
	var an answerContent
	started := false
	var items []*responsesItem
	byID := map[string]*responsesItem{}
	for data := range streamEvents(stream) {
		event, ok := members(data)
		if !ok {
			continue
		}
		typ, _ := stringOf(event["type"])
		switch {
		case slices.Contains(responsesTerminalEvents, typ):
			if response, ok := members(event["response"]); ok {
				return responseObjectContent(response)
			}
		case typ == "response.created" || typ == "response.in_progress":
			response, ok := members(event["response"])
			if !ok {
				continue
			}
			started = true
			if an.id == "" {
				an.id, _ = stringOf(response["id"])
			}
			if an.model == "" {
				an.model, _ = stringOf(response["model"])
			}
		case typ == "response.output_item.added" || typ == "response.output_item.done":
			item, ok := members(event["item"])
			if !ok {
				continue
			}
			id, ok := stringOf(item["id"])
			if !ok {
				continue
			}
			started = true
			it := byID[id]
			if it == nil {
				it = &responsesItem{}
				byID[id] = it
				items = append(items, it)
				it.begin(event["item"], item)
			}
			if typ == "response.output_item.done" {
				it.done = event["item"]
			}
		default:
			id, _ := stringOf(event["item_id"])
			if it := byID[id]; it != nil && it.done == nil {
				it.add(typ, event)
			}
		}
	}
	if !started {
		return answerContent{}
	}
	var parts []part
	for _, it := range items {
		parts = it.appendTo(parts)
	}
	an.messages = []message{newMessage("assistant", "", parts)}
	an.finishReasons = []string{""}
	return an
}

// responsesItem is one output item of a stream without its terminal event: as
// response.output_item.done finished it, or, still in progress, as its deltas have
// built it — a message's text and refusal parts, a reasoning item's summary and
// reasoning text parts, a function call's arguments — and any other item as
// response.output_item.added began it.
type responsesItem struct {
	// added is the item as it began; typ its type.
	added json.RawMessage
	typ   string
	// callID, name and hasName are a function call's, from the item as it began.
	callID, name string
	hasName      bool
	// hasArguments: the call began with arguments or a fragment of them arrived.
	hasArguments bool
	// done is the item as response.output_item.done finished it; nil while in
	// progress.
	done json.RawMessage
	// parts are a message's or a reasoning item's parts in progress, in the order
	// they began, and texts what their deltas built.
	parts     []responsesPart
	texts     map[responsesPart]*strings.Builder
	arguments strings.Builder
}

// responsesPart names one part of an item in progress: the event type of its deltas
// and the index they give it — content_index, or a reasoning summary's
// summary_index. Deltas without one (llama-server's carry none) build one part of
// their type, hasIndex false.
type responsesPart struct {
	kind     string
	index    int64
	hasIndex bool
}

// begin starts the item as response.output_item.added — or, for an item never
// added, response.output_item.done — gave it (raw, its members item).
func (it *responsesItem) begin(raw json.RawMessage, item map[string]json.RawMessage) {
	it.added = raw
	it.typ, _ = stringOf(item["type"])
	if it.typ != "function_call" {
		return
	}
	it.callID, _ = stringOf(item["call_id"])
	it.name, it.hasName = stringOf(item["name"])
	if s, ok := stringOf(item["arguments"]); ok {
		it.arguments.WriteString(s)
		it.hasArguments = true
	}
}

// add adds an in-progress item's delta event of type typ to what its type builds:
// the part the delta's index names, or a function call's arguments. A delta of any
// other event, or for an item of another type, is not read.
func (it *responsesItem) add(typ string, event map[string]json.RawMessage) {
	delta, _ := stringOf(event["delta"])
	switch {
	case typ == "response.output_text.delta" && it.typ == "message",
		typ == "response.refusal.delta" && it.typ == "message",
		typ == "response.reasoning_text.delta" && it.typ == "reasoning":
		it.text(typ, event["content_index"]).WriteString(delta)
	case typ == "response.reasoning_summary_text.delta" && it.typ == "reasoning":
		it.text(typ, event["summary_index"]).WriteString(delta)
	case typ == "response.function_call_arguments.delta" && it.typ == "function_call":
		it.arguments.WriteString(delta)
		it.hasArguments = true
	}
}

// text is the text of the item's part that deltas of type kind name by index: the
// part already begun, or a new one after the others. A delta whose index is not an
// integer names the part without one.
func (it *responsesItem) text(kind string, index json.RawMessage) *strings.Builder {
	key := responsesPart{kind: kind}
	key.index, key.hasIndex = integerOf(index)
	if b := it.texts[key]; b != nil {
		return b
	}
	if it.texts == nil {
		it.texts = map[responsesPart]*strings.Builder{}
	}
	b := &strings.Builder{}
	it.texts[key] = b
	it.parts = append(it.parts, key)
	return b
}

// appendTo appends the item's parts: a finished item's as the same item unstreamed
// gives them; an item in progress's in the order its finished item's would be — a
// message's text and refusal parts in the order they began, a reasoning item's
// summary parts, then its reasoning text parts, each in the order they began; a
// tool call's half-written arguments as the string.
func (it *responsesItem) appendTo(parts []part) []part {
	if it.done != nil {
		return appendResponsesOutputItem(parts, it.done)
	}
	switch it.typ {
	case "message":
		for _, key := range it.parts {
			parts = appendText(parts, it.texts[key].String())
		}
		return parts
	case "reasoning":
		for _, kind := range []string{"response.reasoning_summary_text.delta", "response.reasoning_text.delta"} {
			for _, key := range it.parts {
				if key.kind == kind {
					parts = appendReasoning(parts, it.texts[key].String())
				}
			}
		}
		return parts
	case "function_call":
		if !it.hasName {
			return parts
		}
		var arguments any
		if it.hasArguments {
			arguments = stringArguments(it.arguments.String())
		}
		return append(parts, newToolCallPart(it.callID, it.name, arguments))
	}
	return appendResponsesOutputItem(parts, it.added)
}
