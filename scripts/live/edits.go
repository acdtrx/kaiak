package main

// The image edit checks, with -edit-base-url: a vLLM-Omni server serving an image
// edit model (Qwen-Image-Edit), sent an upload as OpenAI's image edit API takes it in
// multipart form — a prompt and a PNG drawn here (uploads.go).

import (
	"fmt"
	"net/http"
	"strings"
)

// Request IDs of the edit checks' requests whose log lines they read.
const (
	idEdit             = "live-edit"
	idEditStream       = "live-edit-stream"
	idEditURL          = "live-edit-url"
	idEditLoRA         = "live-edit-lora"
	idGenerationToEdit = "live-generation-to-edit"
)

// editPrompt is what the edit checks ask for.
const editPrompt = "Paint the lighthouse red."

// editForm is an edit request to model: editPrompt on editImage, few steps — the
// checks need an image, not a good one — with params added as text parts.
func editForm(model string, params map[string]any) []formPart {
	parts := []formPart{textPart("model", model), textPart("prompt", editPrompt),
		filePart("image", "lighthouse.png", "image/png", editImage()),
		textPart("num_inference_steps", "10"), textPart("seed", "7")}
	return withTextParts(parts, params)
}

func (r *run) editChecks() {
	if images, ok := r.checkEdit(); ok {
		r.checkMediaLog("usage-log/edit", idEdit, mediaLog{outputType: "image", estimated: true, images: images})
	}
	r.checkEditRefused()
	r.checkGenerationToEdit()
}

// checkEdit edits the lighthouse (with the run's imageCheckParams: the self-test asks
// for more than one image): b64_json data whose every element decodes to a PNG, JPEG
// or WebP. It returns how many the answer holds.
func (r *run) checkEdit() (int64, bool) {
	const name = "edit"
	resp, err := r.postForm(r.key, "/v1/images/edits", idEdit, editForm(modelEdit, r.o.imageCheckParams))
	if !r.ok(name, resp, err, idEdit) {
		return 0, false
	}
	formats, problem := imagesAnswer(resp.body)
	if problem != "" {
		r.fail(name, "%s", problem)
		return 0, false
	}
	r.pass(name, fmt.Sprintf("a 512×512 PNG in, %d image(s) out: %s", len(formats), strings.Join(formats, ", ")))
	return int64(len(formats)), true
}

// checkEditRefused: a stream, an input image by URL — which vLLM-Omni would fetch —
// and a LoRA it would load from a server path are refused before the backend.
func (r *run) checkEditRefused() {
	edits := "/v1/images/edits"
	r.checkRefusals("edit-refused", []refusalCase{
		{what: "stream=true", path: edits, id: idEditStream, body: editForm(modelEdit, map[string]any{"stream": "true"}),
			status: http.StatusBadRequest, code: "invalid_value", param: "stream"},
		{what: "url by https", path: edits, id: idEditURL,
			body:   editForm(modelEdit, map[string]any{"url": "https://example.com/lighthouse.png"}),
			status: http.StatusBadRequest, code: "external_reference_unsupported", param: "url"},
		{what: "lora", path: edits, id: idEditLoRA,
			body:   editForm(modelEdit, map[string]any{"lora": `{"name":"style","path":"/models/lora/style"}`}),
			status: http.StatusBadRequest, code: "external_reference_unsupported", param: "lora"},
	})
}

// checkGenerationToEdit sends an image generation to live-edit, whose endpoints list
// names edits alone: the gateway's own 400 endpoint_not_served, never sent — where
// vLLM-Omni answers a generation on an edit model 500, a backend failure toward the
// circuit (docs/specs/GATEWAY.md, Model metadata → Endpoints per model).
func (r *run) checkGenerationToEdit() {
	r.checkRefusals("generation-to-edit", []refusalCase{{what: "image generation to " + modelEdit,
		path: "/v1/images/generations", id: idGenerationToEdit, body: imageBody(modelEdit, nil),
		status: http.StatusBadRequest, code: "endpoint_not_served"}})
}
