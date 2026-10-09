package fakebackend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Image generation, as vLLM-Omni serves it (vllm_omni/entrypoints/openai/api_server.py
// and protocol/images.py, v0.30.0), at /v1/images/generations: a model with no
// diffusion stage — a speech model (SetNonImageModels) — answers 503 "No diffusion
// stage found in multi-stage pipeline."; the request's model is compared with the
// server's first served name (SetImageServedModel), another answering 400 "Model
// mismatch: request specifies '<model>' but server is running '<name>'."; n is 1 to 10
// (absent 1) and response_format b64_json (absent means it), anything else 400. The
// answer is a JSON body, {"created", "data": [{"b64_json", "url": null,
// "revised_prompt": null}], "output_format", "size", "cot_output", "metrics"} — no
// model, no usage — each image ImageBytes of PNG-signed filler, base64. A request's
// layers makes the answer hold that many images per image asked for, as a layered
// model answers one image per layer. The reply's stream fault stops the answer within
// image At's base64: Cut drops the connection, Hang waits for the request's end, End
// ends the body there.

// imagesPath is the image generation endpoint, below the version prefix.
const imagesPath = "images/generations"

// DefaultImageBytes is each image's size when the Reply sets no ImageBytes.
const DefaultImageBytes = 1024

// imagesMaxN is the most images vLLM-Omni's request admits (n).
const imagesMaxN = 10

// pngSignature opens every image the fake answers.
const pngSignature = "\x89PNG\r\n\x1a\n"

// SetImageServedModel scripts, from now on, the server's first served name, which
// image generation compares a request's model with: another model is answered 400
// "Model mismatch" ("": every model is the server's).
func (b *Backend) SetImageServedModel(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.imageModel = name
}

// SetNonImageModels scripts, from now on, the models whose image generation the
// backend answers as vLLM-Omni answers it on a model with no diffusion stage: 503, "No
// diffusion stage found in multi-stage pipeline." (nil: every model draws).
func (b *Backend) SetNonImageModels(models ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nonImage = models
}

// writeImages answers an image generation request for model.
func (b *Backend) writeImages(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	top map[string]json.RawMessage, model, imageModel string, nonImage []string) {
	if slices.Contains(nonImage, model) {
		writeJSON(w, http.StatusServiceUnavailable, omniError(http.StatusServiceUnavailable, "Service Unavailable",
			"No diffusion stage found in multi-stage pipeline."))
		return
	}
	if imageModel != "" && model != imageModel {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("Model mismatch: request specifies '%s' but server is running '%s'.", model, imageModel)))
		return
	}
	n := 1
	if raw, ok := top["n"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > imagesMaxN {
			writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
				fmt.Sprintf("n: Input should be between 1 and %d", imagesMaxN)))
			return
		}
	}
	format := "b64_json"
	_ = json.Unmarshal(top["response_format"], &format) // absent or null: b64_json
	if format != "b64_json" {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			fmt.Sprintf("the fake does not play response_format %q", format)))
		return
	}
	layers := 1
	if json.Unmarshal(top["layers"], &layers) != nil || layers < 1 {
		layers = 1
	}
	size := "1024x1024"
	_ = json.Unmarshal(top["size"], &size) // a size that is not a string: the default

	imageBytes := reply.ImageBytes
	if imageBytes == 0 {
		imageBytes = DefaultImageBytes
	}
	image := []byte(pngSignature + strings.Repeat("\x00", max(imageBytes-len(pngSignature), 0)))
	element := `{"b64_json":"` + base64.StdEncoding.EncodeToString(image[:imageBytes]) + `","url":null,"revised_prompt":null}`
	images := n * layers
	head := `{"created":` + strconv.Itoa(created) + `,"data":[`
	sizeJSON, _ := json.Marshal(size) // a string always encodes
	tail := `],"output_format":"png","size":` + string(sizeJSON) + `,"cot_output":null,"metrics":{}}`

	w.Header().Set("Content-Type", "application/json")
	fault := reply.Fault
	if fault == nil || fault.At >= images {
		w.Header().Set("Content-Length", strconv.Itoa(len(head)+images*len(element)+images-1+len(tail)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(head))
		for i := range images {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(element))
		}
		_, _ = w.Write([]byte(tail))
		return
	}
	// The fault: the images before At, and half of image At.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(head + strings.Repeat(element+",", fault.At) + element[:len(element)/2]))
	_ = http.NewResponseController(w).Flush()
	switch fault.Kind {
	case Cut:
		panic(http.ErrAbortHandler)
	case Hang:
		b.waitForCancel(r, req)
	case End:
	default:
		panic(fmt.Sprintf("fakebackend: an image answer's fault of Kind %d", fault.Kind))
	}
}
