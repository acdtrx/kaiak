package e2e

// Image generation end to end (docs/specs/GATEWAY.md, Client API → owned fields:
// images; Providers → media answers; Accounting → Media usage; Limits → Check and
// reserve): one gateway passes image requests to a vllm-omni backend — the fake
// backend playing vLLM-Omni — with the model edit alone; the answer comes back as the
// backend sent it, never a stream; each request settles the images of the answer's
// data[] — a layered model's more than n — and its prompt's tokens estimated, under the
// operation generate_content and the output type image, on the request line and in
// the usage metrics. An image limit reserves n and settles the answer's count, so a
// layered answer takes it past its value; it refuses image requests alone. What the
// gateway refuses — url and file answers, a LoRA, more images than the cap — never
// reaches the backend, and nothing of the prompt or the images reaches the log.

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// imageAnswer is an image answer as the client gets it: its members, and each image
// decoded.
func imageAnswer(t *testing.T, r *response) (members string, images [][]byte) {
	t.Helper()
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d (%s) %.300s, want 200 with a JSON body", r.StatusCode, r.Header.Get("Content-Type"), r.body)
	}
	var answer struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.body, &answer); err != nil {
		t.Fatalf("image answer %.300s: %v", r.body, err)
	}
	for i, d := range answer.Data {
		image, err := base64.StdEncoding.DecodeString(d.B64JSON)
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		images = append(images, image)
	}
	return strings.Join(slices.Sorted(maps.Keys(r.json(t))), ","), images
}

// pngSignature opens every image the fake backend draws.
const pngSignature = "\x89PNG\r\n\x1a\n"

func TestImages(t *testing.T) {
	g, fake, key := startOmni(t)

	// Two images of 2000 bytes: the request sent with the model edit alone — its
	// stream too, which image generation does not have — the answer relayed in
	// vLLM-Omni's shape, never flushed as a stream; the record settles the two images
	// and the prompt's input estimate, flagged, no output, no cost.
	t.Run("images", func(t *testing.T) {
		fake.SetReply(fakebackend.Reply{ImageBytes: 2000})
		defer fake.SetReply(fakebackend.Reply{})
		body := `{"model":"painter","prompt":"a red fox in snow","negative_prompt":"blur","n":2,"size":"512x512","stream":true}`
		before := len(fake.Requests())
		r := g.post(t, "/v1/images/generations", key, "images-two", json.RawMessage(body))
		members, images := imageAnswer(t, r)
		if members != "cot_output,created,data,metrics,output_format,size" {
			t.Errorf("answer members %s, want vLLM-Omni's", members)
		}
		if len(images) != 2 || len(images[0]) != 2000 || len(images[1]) != 2000 || !strings.HasPrefix(string(images[0]), pngSignature) {
			t.Errorf("%d images: want two PNG images of 2000 bytes", len(images))
		}
		if r.Header.Get("X-Accel-Buffering") != "" {
			t.Error("an image answer was flushed as a stream")
		}

		reqs := fake.Requests()
		if len(reqs) != before+1 {
			t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
		}
		want := strings.Replace(body, `"model":"painter"`, `"model":"`+omniImageModel+`"`, 1)
		if got := reqs[len(reqs)-1]; got.Path != "/v1/images/generations" || string(got.Body) != want {
			t.Errorf("backend got %s\n %s\nwant /v1/images/generations\n %s", got.Path, got.Body, want)
		}
		estimate := accounting.EstimateInput(provider.ImagesGenerations, []byte(body)).Total
		wantLine(t, g.settled(t, "images-two"), map[string]any{"kaiak.backend.id": "omni", "kaiak.backend.type": "vllm-omni",
			"gen_ai.operation.name": "generate_content", "gen_ai.output.type": "image", "gen_ai.request.stream": false,
			"kaiak.usage.images": 2.0, "gen_ai.usage.input_tokens": float64(estimate), "gen_ai.usage.output_tokens": 0.0,
			"kaiak.usage.cost_usd": 0.0, "kaiak.usage.estimated": true, "kaiak.usage.partial": false})
	})

	// A layered model answers one image per layer: n 1 with three layers is three
	// images, all relayed and all counted.
	t.Run("a layered model's answer", func(t *testing.T) {
		r := g.post(t, "/v1/images/generations", key, "images-layered",
			json.RawMessage(`{"model":"painter","prompt":"a fox","n":1,"layers":3}`))
		if _, images := imageAnswer(t, r); len(images) != 3 {
			t.Errorf("%d images, want three", len(images))
		}
		wantLine(t, g.settled(t, "images-layered"), map[string]any{"kaiak.usage.images": 3.0, "kaiak.usage.partial": false})
	})

	// url and file answers, a LoRA (an adapter path the server would load) and more
	// images than global.max_images_per_request are refused before any backend, in
	// OpenAI's shape, naming the member at fault and never echoing its value (Client
	// API → errors).
	t.Run("refused before any backend", func(t *testing.T) {
		before := len(fake.Requests())
		for _, c := range []struct {
			name, body, code, param string
		}{
			{"url", `{"model":"painter","prompt":"a fox","response_format":"url"}`, "response_format_unsupported", "response_format"},
			{"file", `{"model":"painter","prompt":"a fox","response_format":"file"}`, "response_format_unsupported", "response_format"},
			{"lora", `{"model":"painter","prompt":"a fox","lora":{"name":"style","path":"/srv/adapters/style","scale":1}}`,
				"external_reference_unsupported", "lora"},
			{"more images than the cap", `{"model":"painter","prompt":"a fox","n":5}`, "invalid_value", "n"},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := g.post(t, "/v1/images/generations", key, "", json.RawMessage(c.body))
				e, _ := r.json(t)["error"].(map[string]any)
				if r.StatusCode != http.StatusBadRequest || e["code"] != c.code || e["param"] != c.param {
					t.Errorf("%d %s, want 400 %s on %s", r.StatusCode, r.body, c.code, c.param)
				}
				if strings.Contains(string(r.body), "/srv/") {
					t.Errorf("the refusal echoes the LoRA's path: %s", r.body)
				}
			})
		}
		if n := len(fake.Requests()) - before; n != 0 {
			t.Errorf("the backend got %d requests for refused ones", n)
		}
	})

	// The group's 5 images a minute: 3 reserved and settled; 3 more refused, 429
	// images; a layered answer admitted on its n of 1 settles its 3 images, taking the
	// window to 6 — one answer's overshoot — and the next image is refused. Chat and
	// speech in the same group are not.
	t.Run("images per minute", func(t *testing.T) {
		refused := func(id string, body json.RawMessage) {
			t.Helper()
			r := g.post(t, "/v1/images/generations", imagesKey, id, body)
			e, _ := r.json(t)["error"].(map[string]any)
			if r.StatusCode != http.StatusTooManyRequests || e["code"] != "rate_limit_exceeded" || e["type"] != "images" ||
				r.Header.Get("Retry-After") == "" {
				t.Fatalf("%s: %d %s (Retry-After %q), want 429 images with a wait", id, r.StatusCode, r.body, r.Header.Get("Retry-After"))
			}
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.group": "images",
				"kaiak.limit.type": "images_per_minute", "kaiak.limit.configured": 5.0})
		}
		three := json.RawMessage(`{"model":"painter","prompt":"a fox","n":3}`)
		if _, images := imageAnswer(t, g.post(t, "/v1/images/generations", imagesKey, "images-limit-three", three)); len(images) != 3 {
			t.Fatalf("%d images, want three", len(images))
		}
		g.settled(t, "images-limit-three")
		refused("images-limit-three-more", three)
		r := g.post(t, "/v1/images/generations", imagesKey, "images-limit-layered",
			json.RawMessage(`{"model":"painter","prompt":"a fox","n":1,"layers":3}`))
		if _, images := imageAnswer(t, r); len(images) != 3 {
			t.Fatalf("%d images, want three", len(images))
		}
		wantLine(t, g.settled(t, "images-limit-layered"), map[string]any{"kaiak.usage.images": 3.0})
		refused("images-limit-one-more", json.RawMessage(`{"model":"painter","prompt":"a fox"}`))
		if got := g.metric(t, `kaiak_limit_rejections_total{kaiak_limit_scope="group",kaiak_limit_type="images_per_minute"}`); got != 2 {
			t.Errorf("image limit rejections = %v, want 2", got)
		}
		if r := g.post(t, "/v1/chat/completions", imagesKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Errorf("chat in the group: %d %s, want 200", r.StatusCode, r.body)
		}
		speech := json.RawMessage(`{"model":"tts","input":"hi"}`)
		if r := g.post(t, "/v1/audio/speech", imagesKey, "images-limit-speech", speech); r.StatusCode != http.StatusOK {
			t.Errorf("speech in the group: %d %.300s, want 200", r.StatusCode, r.body)
		}
		g.settled(t, "images-limit-speech")
	})

	// The usage metrics count the images generated per key and model, under
	// generate_content and image: 2 and the layered 3 for k, 3 and the layered 3 for
	// k-images; the speech request in the images group adds none.
	usage := func(key, group, model, outputType string) string {
		return `{kaiak_key_group="` + group + `",kaiak_key_root_group="` + group + `",kaiak_key_id="` + key +
			`",gen_ai_request_model="` + model + `",gen_ai_operation_name="generate_content",gen_ai_output_type="` + outputType +
			`",kaiak_usage_status="complete"}`
	}
	for series, want := range map[string]float64{
		`kaiak_usage_images_total` + usage("k", "w", "painter", "image"):             5,
		`kaiak_usage_images_total` + usage("k-images", "images", "painter", "image"): 6,
		`kaiak_usage_records_total` + usage("k", "w", "painter", "image"):            2,
		`kaiak_usage_records_total` + usage("k-images", "images", "tts", "speech"):   1,
	} {
		if got := g.metric(t, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if v, ok := g.metricValue(t, `kaiak_usage_images_total`+usage("k-images", "images", "tts", "speech")); ok {
		t.Errorf("speech counted %v images", v)
	}
	// Nothing of the prompts, the LoRA's path or the images reaches the log
	// ("iVBORw0KGgo": the PNG signature in base64, which opens every image).
	for _, content := range []string{"a red fox", "/srv/adapters", "iVBORw0KGgo"} {
		if strings.Contains(g.logs.text(), content) {
			t.Errorf("the log holds %q", content)
		}
	}
	g.stop(t)
}
