package e2e

// Image edits end to end (docs/specs/GATEWAY.md, Client API → owned parts: image edits;
// Request pipeline → Uploads; Accounting → Media usage: images; Limits → Check and
// reserve): one gateway passes an edit — a multipart/form-data upload — to a vllm-omni
// backend, the fake backend playing vLLM-Omni. The backend gets the form rebuilt: every
// part as the client wrote it but model, the deployment's, under a boundary of the
// gateway's with its exact length. The answer comes back as the backend sent it, never
// a stream, and the request settles the images of the answer's data[] and its input
// estimated — the prompt and each input image — under the operation generate_content
// and the output type image, on the request line and in the usage metrics. An image
// limit reserves an edit's n. What the gateway refuses — a body that is no form, an
// upload over the cap, an edit to a model whose endpoints list leaves edits out — never
// reaches the backend, and nothing of the prompt, the filenames or the images reaches
// the log.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
)

// omniEditModel is the backend-side model of the edit scenario's editor.
const omniEditModel = "Qwen/Qwen-Image-Edit-2511"

// editsUploadCap is the edit scenario's global.max_upload_bytes: 64 KiB, which every
// edit it sends fits.
const editsUploadCap = 64 << 10

// editsConfig is a config with two vllm-omni backends on the fake backend at fakeURL,
// one server per model: "editor" (on omni-edit as omniEditModel), whose endpoints list
// names image edits alone, and "painter" (on omni-image as omniImageModel), image
// generation alone; global.max_images_per_request 4 and max_upload_bytes
// editsUploadCap; the group w with the key k of hash, and the group images
// (images_per_minute 5) with the key k-images of imagesHash.
func editsConfig(fakeURL, hash, imagesHash string) map[string]any {
	model := func(backend, deployed string, endpoints ...any) map[string]any {
		return map[string]any{
			"deployments": []any{map[string]any{"backend": backend, "model": deployed}},
			"endpoints":   endpoints,
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
		}
	}
	return map[string]any{
		"protocol_version": 8,
		"global":           map[string]any{"max_images_per_request": 4, "max_upload_bytes": editsUploadCap},
		"backends": map[string]any{
			"omni-edit":  map[string]any{"type": "vllm-omni", "base_url": fakeURL + "/v1"},
			"omni-image": map[string]any{"type": "vllm-omni", "base_url": fakeURL + "/v1"},
		},
		"models": map[string]any{
			"editor":  model("omni-edit", omniEditModel, "images_edits"),
			"painter": model("omni-image", omniImageModel, "images_generations"),
		},
		"groups": map[string]any{
			"w":      map[string]any{"capture": false, "allowed_models": []any{"*"}},
			"images": map[string]any{"capture": false, "allowed_models": []any{"*"}, "limits": []any{map[string]any{"type": "images_per_minute", "value": 5}}},
		},
		"keys": map[string]any{
			"k":        map[string]any{"hash": hash, "group": "w"},
			"k-images": map[string]any{"hash": imagesHash, "group": "images"},
		},
	}
}

// editForm is an edit of the editor as a client sends it: model, prompt, one input
// image and n, then extra.
func editForm(t *testing.T, n int, extra ...formPart) (string, []byte) {
	t.Helper()
	parts := []formPart{textPart("model", "editor"), textPart("prompt", "make the fox red"),
		filePart("image", "fox.png", "image/png", pngImage), textPart("n", strconv.Itoa(n))}
	return writeForm(t, append(parts, extra...)...)
}

func TestImageEdits(t *testing.T) {
	fake := fakebackend.New()
	t.Cleanup(fake.Close)
	fake.SetModels(omniEditModel, omniImageModel)
	key, hash := newKey()
	limitedKey, limitedHash := newKey()
	configFile := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, configFile, editsConfig(fake.URL(), hash, limitedHash))
	g := startGateway(t, configFile)

	// Two images of 2000 bytes from two input images and an inline one: the backend
	// gets the form rebuilt — the list parts repeated, the inline image, OpenAI's mask
	// and the parts the gateway does not read as the client wrote them — and its answer
	// is relayed in vLLM-Omni's shape, never flushed as a stream; the request settles
	// the two images and the prompt and three input images estimated, flagged, no
	// output, no cost.
	t.Run("an edit through the pipeline", func(t *testing.T) {
		fake.SetReply(fakebackend.Reply{ImageBytes: 2000})
		defer fake.SetReply(fakebackend.Reply{})
		const prompt = "a red fox in snow"
		contentType, body := writeForm(t, textPart("model", "editor"), textPart("prompt", prompt),
			filePart("image", "fox one.png", "image/png", pngImage), filePart("image", "fox ünï.png", "image/png", pngImage),
			textPart("url[]", "data:image/png;base64,iVBORw0KGgo="), filePart("mask", "mask.png", "image/png", pngImage),
			textPart("n", "2"), textPart("size", "512x512"), textPart("response_format", "b64_json"),
			textPart("negative_prompt", "blur"), textPart("x_custom", "kept"))
		before := len(fake.Requests())
		r := g.postForm(t, "/v1/images/edits", key, "edit-two", contentType, body)
		members, images := imageAnswer(t, r)
		if members != "cot_output,created,data,metrics,output_format,size" {
			t.Errorf("answer members %s, want vLLM-Omni's", members)
		}
		if len(images) != 2 || len(images[0]) != 2000 || len(images[1]) != 2000 || !strings.HasPrefix(string(images[0]), pngSignature) {
			t.Errorf("%d images: want two PNG images of 2000 bytes", len(images))
		}
		if r.Header.Get("X-Accel-Buffering") != "" {
			t.Error("an edit's answer was flushed as a stream")
		}

		reqs := fake.Requests()
		if len(reqs) != before+1 {
			t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
		}
		sent := reqs[len(reqs)-1]
		if sent.Path != "/v1/images/edits" {
			t.Fatalf("backend got %s, want /v1/images/edits", sent.Path)
		}
		wantRebuilt(t, sent, contentType, body, omniEditModel)

		estimate := accounting.EstimateUploadInput(int64(len(prompt)), 3).Total
		wantLine(t, g.settled(t, "edit-two"), map[string]any{"kaiak.backend.id": "omni-edit", "kaiak.backend.type": "vllm-omni",
			"kaiak.deployment.model": omniEditModel, "gen_ai.operation.name": "generate_content", "gen_ai.output.type": "image",
			"gen_ai.request.stream": false, "kaiak.usage.images": 2.0, "gen_ai.usage.input_tokens": float64(estimate),
			"gen_ai.usage.output_tokens": 0.0, "kaiak.usage.cost_usd": 0.0, "kaiak.usage.estimated": true,
			"kaiak.usage.partial": false})
	})

	// A body that is no form — OpenAI's JSON form of edits, a form without its
	// boundary — an upload over max_upload_bytes, declared or not, an edit to a model
	// whose list names image generation alone, and an image generation to the editor,
	// whose list names edits alone: each refused in OpenAI's shape before any backend.
	t.Run("refused before any backend", func(t *testing.T) {
		before := len(fake.Requests())
		t.Run("OpenAI's JSON form", func(t *testing.T) {
			r := g.post(t, "/v1/images/edits", key, "", json.RawMessage(
				`{"model":"editor","prompt":"make the fox red","images":[{"image_url":"data:image/png;base64,iVBORw0KGgo="}]}`))
			wantRefusal(t, r, http.StatusBadRequest, "unsupported_content_type")
		})
		t.Run("a form without its boundary", func(t *testing.T) {
			_, body := editForm(t, 1)
			wantRefusal(t, g.postForm(t, "/v1/images/edits", key, "", "multipart/form-data", body),
				http.StatusBadRequest, "unsupported_content_type")
		})
		over := filePart("image[]", "large.png", "image/png", strings.Repeat("x", editsUploadCap))
		wantMessage := fmt.Sprintf("The request body exceeds the limit of %d bytes.", editsUploadCap)
		t.Run("over the upload cap", func(t *testing.T) {
			contentType, body := editForm(t, 1, over)
			e := wantRefusal(t, g.postForm(t, "/v1/images/edits", key, "", contentType, body),
				http.StatusRequestEntityTooLarge, "request_too_large")
			if e["message"] != wantMessage {
				t.Errorf("message %q, want %q", e["message"], wantMessage)
			}
		})
		t.Run("over the upload cap, its length undeclared", func(t *testing.T) {
			contentType, body := editForm(t, 1, over)
			e := wantRefusal(t, g.postUpload(t, "/v1/images/edits", key, "", contentType, io.MultiReader(strings.NewReader(string(body)))),
				http.StatusRequestEntityTooLarge, "request_too_large")
			if e["message"] != wantMessage {
				t.Errorf("message %q, want %q", e["message"], wantMessage)
			}
		})
		t.Run("an edit to a model not listing edits", func(t *testing.T) {
			contentType, body := writeForm(t, textPart("model", "painter"), textPart("prompt", "make the fox red"),
				filePart("image", "fox.png", "image/png", pngImage))
			e := wantRefusal(t, g.postForm(t, "/v1/images/edits", key, "", contentType, body), http.StatusBadRequest, "endpoint_not_served")
			if want := "The model is not served on /v1/images/edits: its configured endpoints do not include this API."; e["message"] != want {
				t.Errorf("message %q, want %q", e["message"], want)
			}
		})
		t.Run("an image generation to the editor", func(t *testing.T) {
			r := g.post(t, "/v1/images/generations", key, "", json.RawMessage(`{"model":"editor","prompt":"a fox"}`))
			wantRefusal(t, r, http.StatusBadRequest, "endpoint_not_served")
		})
		if n := len(fake.Requests()) - before; n != 0 {
			t.Errorf("the backend got %d requests for refused ones", n)
		}
	})

	// The group's 5 images a minute: an edit's n is reserved — 3 settled, then 3 more
	// refused, 429 images, with only 3 used; 2 more admitted, filling the window, and 1
	// more refused.
	t.Run("images per minute", func(t *testing.T) {
		edit := func(id string, n int) *response {
			contentType, body := editForm(t, n)
			return g.postForm(t, "/v1/images/edits", limitedKey, id, contentType, body)
		}
		served := func(id string, n int) {
			t.Helper()
			if _, images := imageAnswer(t, edit(id, n)); len(images) != n {
				t.Fatalf("%s: %d images, want %d", id, len(images), n)
			}
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.usage.images": float64(n)})
		}
		refused := func(id string, n, used int) {
			t.Helper()
			before := len(fake.Requests())
			r := edit(id, n)
			e, _ := r.json(t)["error"].(map[string]any)
			if r.StatusCode != http.StatusTooManyRequests || e["code"] != "rate_limit_exceeded" || e["type"] != "images" ||
				r.Header.Get("Retry-After") == "" {
				t.Fatalf("%s: %d %.300s (Retry-After %q), want 429 images with a wait", id, r.StatusCode, r.body, r.Header.Get("Retry-After"))
			}
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.group": "images",
				"kaiak.limit.type": "images_per_minute", "kaiak.limit.configured": 5.0, "kaiak.limit.used": float64(used)})
			if got := len(fake.Requests()); got != before {
				t.Errorf("%s: the backend got the refused edit", id)
			}
		}
		served("edits-limit-three", 3)
		refused("edits-limit-three-more", 3, 3)
		served("edits-limit-two", 2)
		refused("edits-limit-one-more", 1, 5)
		if got := g.metric(t, `kaiak_limit_rejections_total{kaiak_limit_scope="group",kaiak_limit_type="images_per_minute"}`); got != 2 {
			t.Errorf("image limit rejections = %v, want 2", got)
		}
	})

	// The usage metrics count the images edited per key, under generate_content and
	// image, on the route /v1/images/edits.
	usage := func(key, group string) string {
		return `{kaiak_key_group="` + group + `",kaiak_key_root_group="` + group + `",kaiak_key_id="` + key +
			`",gen_ai_request_model="editor",gen_ai_operation_name="generate_content",gen_ai_output_type="image",` +
			`gen_ai_provider_name="vllm-omni",kaiak_usage_status="complete"}`
	}
	for series, want := range map[string]float64{
		`kaiak_usage_images_total` + usage("k", "w"):              2,
		`kaiak_usage_images_total` + usage("k-images", "images"):  5,
		`kaiak_usage_records_total` + usage("k-images", "images"): 2,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/images/edits",` +
			`http_response_status_code="200",gen_ai_request_model="editor"}`: 3,
	} {
		if got := g.metric(t, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	// Nothing of the prompts, the filenames or the images ("iVBORw0KGgo": the PNG
	// signature in base64, which opens every image) reaches the log.
	for _, content := range []string{"a red fox", "make the fox", "fox one.png", "fox ünï.png", "mask.png", "iVBORw0KGgo"} {
		if strings.Contains(g.logs.text(), content) {
			t.Errorf("the log holds %q", content)
		}
	}
	g.stop(t)
}
