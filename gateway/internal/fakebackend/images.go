package fakebackend

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
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
//
// Image edits, as vLLM-Omni serves them (api_server.py and images/helpers.py, v0.30.0;
// docs/plans/media-phase-b/RESEARCH.md), at /v1/images/edits: multipart/form-data only
// — a body that is no form with a boundary reads as an empty form, refused 400 for its
// missing prompt; the fake also refuses an upload sent without its Content-Length (400),
// which a server checking an upload's size from its length could not read. Its model
// part is compared with the server's first served name as on generations; prompt is
// required (400); the input images are the image file parts — else the image[] ones,
// ignored when image is present — and the url and url[] text parts, none at all 422;
// n is at least 1 (absent 1), with no upper bound on edits; response_format b64_json
// only; stream true refused 400, as a single-stage pipeline refuses it; layers as on
// generations. It is answered as a generation is.

// imagesPath is the image generation endpoint, below the version prefix.
const imagesPath = "images/generations"

// imageEditsPath is the image edit endpoint, below the version prefix.
const imageEditsPath = "images/edits"

// DefaultImageBytes is each image's size when the Reply sets no ImageBytes.
const DefaultImageBytes = 1024

// imagesMaxN is the most images vLLM-Omni's request admits (n).
const imagesMaxN = 10

// pngSignature opens every image the fake answers.
const pngSignature = "\x89PNG\r\n\x1a\n"

// SetImageServedModel scripts, from now on, the server's first served name, which
// image generation and edits compare a request's model with: another model is
// answered 400 "Model mismatch" ("": every model is the server's).
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
	b.writeImagesAnswer(w, r, req, reply, n*layers, size)
}

// editForm is an image edit's form as vLLM-Omni's route reads it: its text parts'
// values by name, and how many file parts each name holds.
type editForm struct {
	text  map[string][]string
	files map[string]int
}

// value is the form's text part name, and whether it has one.
func (f editForm) value(name string) (string, bool) {
	if v := f.text[name]; len(v) > 0 {
		return v[0], true
	}
	return "", false
}

// readEditForm reads an upload's body as a multipart/form-data form under the boundary
// its Content-Type names: a body of another type reads as an empty form, as
// Starlette's does; ok is false for a form that does not parse. A part is a file part
// when its Content-Disposition names a filename, even an empty one, as Starlette reads
// it.
func readEditForm(contentType string, body []byte) (form editForm, ok bool) {
	form = editForm{text: map[string][]string{}, files: map[string]int{}}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return form, true
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			return form, true
		}
		if err != nil {
			return form, false
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return form, false
		}
		_, disposition, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return form, false
		}
		name := disposition["name"]
		if _, file := disposition["filename"]; file {
			form.files[name]++
		} else {
			form.text[name] = append(form.text[name], string(data))
		}
	}
}

// writeImageEdits answers an image edit, its body req.Body as received.
func (b *Backend) writeImageEdits(w http.ResponseWriter, r *http.Request, req *Request, reply Reply, imageModel string) {
	if r.ContentLength < 0 {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			"the fake takes an upload with its Content-Length"))
		return
	}
	form, ok := readEditForm(r.Header.Get("Content-Type"), req.Body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError", "the form does not parse"))
		return
	}
	if model, ok := form.value("model"); ok && imageModel != "" && model != imageModel {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "Bad Request",
			fmt.Sprintf("Model mismatch: request specifies '%s' but server is running '%s'.", model, imageModel)))
		return
	}
	if _, ok := form.value("prompt"); !ok {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError", "prompt: Field required"))
		return
	}
	inputs := form.files["image"]
	if inputs == 0 {
		inputs = form.files["image[]"]
	}
	if inputs+len(form.text["url"])+len(form.text["url[]"]) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, omniError(http.StatusUnprocessableEntity, "BadRequestError",
			"Either 'image' or 'url' must be provided"))
		return
	}
	n := 1
	if v, ok := form.value("n"); ok {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError", "n: Input should be at least 1"))
			return
		}
	}
	if v, ok := form.value("response_format"); ok && v != "b64_json" {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			fmt.Sprintf("the fake does not play response_format %q", v)))
		return
	}
	if v, ok := form.value("stream"); ok && strings.EqualFold(v, "true") {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			"Streaming is only supported for multi-stage pipelines"))
		return
	}
	layers := 1
	if v, ok := form.value("layers"); ok {
		if l, err := strconv.Atoi(v); err == nil && l > 1 {
			layers = l
		}
	}
	size, ok := form.value("size")
	if !ok || size == "auto" {
		size = "1024x1024"
	}
	b.writeImagesAnswer(w, r, req, reply, n*layers, size)
}

// writeImagesAnswer writes an image answer holding images, of size, as Reply scripts
// it: each image ImageBytes of PNG-signed filler, its fault breaking it within image At.
func (b *Backend) writeImagesAnswer(w http.ResponseWriter, r *http.Request, req *Request, reply Reply, images int,
	size string) {
	imageBytes := reply.ImageBytes
	if imageBytes == 0 {
		imageBytes = DefaultImageBytes
	}
	image := []byte(pngSignature + strings.Repeat("\x00", max(imageBytes-len(pngSignature), 0)))
	element := `{"b64_json":"` + base64.StdEncoding.EncodeToString(image[:imageBytes]) + `","url":null,"revised_prompt":null}`
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
