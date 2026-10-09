package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// imagesCall is an image generation request of the eval group's key carrying body,
// request ID id.
func imagesCall(body, id string) call {
	return call{method: "POST", path: "/v1/images/generations", key: workloadKey, body: body,
		header: map[string]string{"X-Request-Id": id}}
}

// imageUnits is an image record's units: its estimated input and its images.
func imageUnits(in, images int64) accounting.Units {
	u := units(in, 0, 0, 0, 0)
	u[config.UnitImages] = images
	return u
}

// answeredImages reads an image answer as the client got it: each image, decoded.
func answeredImages(t *testing.T, body []byte) [][]byte {
	t.Helper()
	var answer struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("image answer %.200s: %v", body, err)
	}
	var images [][]byte
	for i, d := range answer.Data {
		image, err := base64.StdEncoding.DecodeString(d.B64JSON)
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
		images = append(images, image)
	}
	return images
}

// An image request passes the whole pipeline to a vllm-omni deployment: sent with the
// model edit alone, everything else as the client wrote it — stream included; answered
// with the backend's JSON as sent, never a stream, never flushed as it arrives; settled
// with the answer's images and the prompt's tokens estimated and flagged, no output —
// the token limits reserving the input estimate alone, image generation having no
// output limit; operation generate_content and output type image on the request line
// and the usage metrics, the request never a stream there and never timed to a first
// token (docs/specs/GATEWAY.md, Client API → owned fields: images; Providers; Limits →
// Check and reserve; Accounting → Media usage; Observability).
func TestImagesThroughThePipeline(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
	})
	g.backend.SetReply(fakebackend.Reply{ImageBytes: 2000})
	body := `{"model":"painter","prompt":"a red fox in snow","negative_prompt":"blur","n":2,"size":"512x512","stream":true,"seed":7}`
	w := do(t, g.h, imagesCall(body, "images-1"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s: %.300s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if images := answeredImages(t, w.Body.Bytes()); len(images) != 2 || len(images[1]) != 2000 ||
		!strings.HasPrefix(string(images[0]), "\x89PNG") {
		t.Errorf("%d images: want two PNG images of 2000 bytes", len(images))
	}
	if w.Header().Get("X-Accel-Buffering") != "" {
		t.Error("an image answer was flushed as it arrived")
	}

	reqs := g.backend.Requests()
	want := strings.Replace(body, `"painter"`, `"Qwen/Qwen-Image"`, 1)
	if len(reqs) != 1 || reqs[0].Path != "/v1/images/generations" || string(reqs[0].Body) != want {
		t.Fatalf("backend requests %+v, want one to /v1/images/generations with\n %s", reqs, want)
	}

	rec := onlyRecord(t, g)
	estimate := accounting.EstimateInput(provider.ImagesGenerations, []byte(body)).Total
	expectUnits(t, rec, imageUnits(estimate, 2), true, false)
	if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.FormatInt(100000-estimate, 10) {
		t.Errorf("remaining tokens at admission %s, want the input estimate of %d reserved alone", got, estimate)
	}
	if rec.Operation != "generate_content" || rec.OutputType != "image" || rec.CostNanoUSD != 0 {
		t.Errorf("record operation %q, output type %q, cost %d; want generate_content, image, 0", rec.Operation,
			rec.OutputType, rec.CostNanoUSD)
	}
	line := logLine(t, g, "images-1")
	for _, want := range []string{`"gen_ai.operation.name":"generate_content"`, `"gen_ai.output.type":"image"`,
		`"gen_ai.request.stream":false`, `"kaiak.backend.type":"vllm-omni"`, `"kaiak.usage.images":2`, `"kaiak.usage.estimated":true`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
	usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="painter",` +
		`gen_ai_operation_name="generate_content",gen_ai_output_type="image",kaiak_usage_status="complete"}`
	text := g.metricsText()
	expectMetricLines(t, text,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/images/generations",`+
			`http_response_status_code="200",gen_ai_request_model="painter"} 1`,
		`kaiak_usage_records_total`+usage+` 1`,
		`kaiak_usage_images_total`+usage+` 2`)
	if strings.Contains(text, `kaiak_time_to_first_token_seconds_count{gen_ai_request_model="painter"`) {
		t.Errorf("an image request was timed to a first token")
	}
}

// What the gateway refuses on /v1/images/generations it refuses before routing, in
// OpenAI's error shape, naming the member at fault and never echoing its value: more
// images than max_images_per_request; a response_format other than b64_json — url and
// file included; lora and extra_params, whatever they hold; owned fields of the wrong
// type or named twice; a model on no vllm-omni deployment (docs/specs/GATEWAY.md,
// Client API → owned fields: images; errors).
func TestImagesRefusals(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	for _, c := range []struct {
		name, body, code, param string
	}{
		{"more images than the cap", `{"model":"painter","prompt":"a fox","n":11}`, "invalid_value", "n"},
		{"url", `{"model":"painter","prompt":"a fox","response_format":"url"}`, "response_format_unsupported", "response_format"},
		{"file", `{"model":"painter","prompt":"a fox","response_format":"file"}`, "response_format_unsupported", "response_format"},
		{"another format", `{"model":"painter","prompt":"a fox","response_format":"B64_JSON"}`, "response_format_unsupported",
			"response_format"},
		{"lora", `{"model":"painter","prompt":"a fox","lora":{"name":"style","path":"/srv/adapters/style","scale":1}}`,
			"external_reference_unsupported", "lora"},
		{"lora as a string", `{"model":"painter","prompt":"a fox","lora":"/srv/adapters/style"}`, "external_reference_unsupported", "lora"},
		{"lora empty", `{"model":"painter","prompt":"a fox","lora":{}}`, "external_reference_unsupported", "lora"},
		{"extra_params naming a path", `{"model":"painter","prompt":"a fox","extra_params":{"control_image":"/srv/images/a.png"}}`,
			"external_reference_unsupported", "extra_params"},
		{"extra_params naming no path", `{"model":"painter","prompt":"a fox","extra_params":{"lora":"x","model":"keep"}}`,
			"external_reference_unsupported", "extra_params"},
		{"extra_params empty", `{"model":"painter","prompt":"a fox","extra_params":{}}`, "external_reference_unsupported", "extra_params"},
		{"extra_params null", `{"model":"painter","prompt":"a fox","extra_params":null}`, "external_reference_unsupported", "extra_params"},
		{"n not an integer", `{"model":"painter","prompt":"a fox","n":"2"}`, "invalid_type", "n"},
		{"n a fraction", `{"model":"painter","prompt":"a fox","n":2.5}`, "invalid_type", "n"},
		{"response_format not a string", `{"model":"painter","prompt":"a fox","response_format":["b64_json"]}`, "invalid_type",
			"response_format"},
		{"n named twice", `{"model":"painter","prompt":"a fox","n":1,"n":9}`, "duplicate_member", "n"},
		{"model missing", `{"prompt":"a fox"}`, "missing_required_parameter", "model"},
		{"not on vllm-omni", `{"model":"open","prompt":"a fox"}`, "endpoint_not_served", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, imagesCall(c.body, "refused"))
			expectError(t, w, http.StatusBadRequest, c.code)
			if _, _, param := openAIError(t, w); (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q: %s", param, c.param, w.Body.String())
			}
			if msg := errorMessage(t, w); strings.Contains(msg, "/srv/") || strings.Contains(msg, "url'") ||
				strings.Contains(msg, "file'") || strings.Contains(msg, "B64_JSON") {
				t.Errorf("message %q echoes the value", msg)
			}
			if c.code == "endpoint_not_served" && !strings.Contains(errorMessage(t, w), "/v1/images/generations") {
				t.Errorf("message %q does not name the endpoint", errorMessage(t, w))
			}
		})
	}
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}

	// The cap is the config's.
	g = newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"global": { `, `"global": { "max_images_per_request": 2, `)
	})
	w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":3}`, "over"))
	expectError(t, w, http.StatusBadRequest, "invalid_value")
	if msg := errorMessage(t, w); !strings.Contains(msg, "'n' asks for 3 images; the gateway's maximum is 2") {
		t.Errorf("message %q", msg)
	}
	if w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":2}`, "at")); w.Code != http.StatusOK {
		t.Errorf("two images under a cap of 2: %d %s", w.Code, w.Body.String())
	}
}

// An image request reserves the images its n asks for: absent or null 1, vLLM-Omni's
// default; below 1 one, for the backend to refuse; its meter is told the same; it is
// never a stream, whatever its stream says (docs/specs/GATEWAY.md, Client API → owned
// fields: images; Limits → Check and reserve).
func TestImagesReserveTheirN(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	for n, want := range map[string]int64{"": 1, `,"n":null`: 1, `,"n":0`: 1, `,"n":-3`: 1, `,"n":1`: 1, `,"n":4`: 4, `,"n":10`: 10} {
		rq := &request{endpoint: bodyEndpoint(provider.ImagesGenerations), snapshot: g.holder.Current(),
			body: []byte(`{"model":"painter","prompt":"a fox","stream":true` + n + `}`)}
		if apiErr := parseOwnedFields(rq); apiErr != nil {
			t.Fatalf("n %q: refused %+v", n, apiErr)
		}
		if got := rq.inbound.Media; !maps.Equal(got, map[config.Measure]int64{config.MeasureImages: want}) ||
			rq.inbound.mediaRequest().Images != want || rq.inbound.Stream {
			t.Errorf("n %q: media %v, the meter told %d, stream %v; want %d images, not a stream", n, got,
				rq.inbound.mediaRequest().Images, rq.inbound.Stream, want)
		}
	}
}

// What the gateway does not own passes untouched, and so do the values it reads when
// they keep to the rules: b64_json, nulls as absent, an n below 1 — counted as one,
// for the backend to refuse — and stream, which an image request does not have, so it
// is timed and logged as no stream (docs/specs/GATEWAY.md, Client API → owned fields:
// images).
func TestImagesPassWhatTheyDoNotOwn(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	for _, body := range []string{
		`{"model":"painter","prompt":"a fox","response_format":"b64_json","n":1}`,
		`{"model":"painter","prompt":"a fox","response_format":null,"n":null,"lora":null}`,
		`{"model":"painter","prompt":"a fox","stream":true,"layers":4,"output_format":"webp","extra":{"lora":"x","model":"keep"},` +
			`"url":"https://example.com/x.png","num_inference_steps":20,"guidance_scale":4.5,"true_cfg_scale":4}`,
	} {
		if w := do(t, g.h, imagesCall(body, "pass")); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
			continue
		}
		reqs := g.backend.Requests()
		if sent, want := string(reqs[len(reqs)-1].Body), strings.Replace(body, `"painter"`, `"Qwen/Qwen-Image"`, 1); sent != want {
			t.Errorf("sent\n %s\nwant\n %s", sent, want)
		}
	}
	if line := logLine(t, g, "pass"); !strings.Contains(line, `"gen_ai.request.stream":false`) {
		t.Errorf("an image request naming stream logged as a stream: %s", line)
	}

	// n below 1 reserves one image, and the backend's refusal is relayed.
	g = newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "images_per_minute", "value": 1 }] }`)
	})
	w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":0}`, "zero"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "n: Input should be between 1 and 10") {
		t.Errorf("n 0: %d %s, want the backend's 400 relayed", w.Code, w.Body.String())
	}
	expectUnits(t, onlyRecord(t, g), imageUnits(0, 0), false, false)
}

// The image limits reserve a request's n before it runs and settle to the answer's
// images: a layered model's answer, more than n, takes the window past its value —
// one answer's overshoot — and the next request is refused, 429 images. Neither limit
// touches chat or speech, which count no images (docs/specs/GATEWAY.md, Limits →
// Check and reserve, Settle).
func TestImageLimits(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitImagesPerMinute, config.LimitImagesPerHour} {
		t.Run(string(typ), func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g, func(doc string) string {
				return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "`+string(typ)+`", "value": 5 }] }`)
			})
			if w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":3}`, "three")); w.Code != http.StatusOK {
				t.Fatalf("3 images under 5: %d %s", w.Code, w.Body.String())
			}
			if used := counterUsed(t, g, "research", typ); used != 3 {
				t.Errorf("window holds %d images, want 3", used)
			}
			w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":3}`, "refused"))
			expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
			if typ, _, _ := openAIError(t, w); typ != "images" || w.Header().Get("Retry-After") == "" {
				t.Errorf("type %q, Retry-After %q: want images and a wait", typ, w.Header().Get("Retry-After"))
			}
			if !strings.Contains(errorMessage(t, w), "group limit of 5 images per") || !strings.Contains(errorMessage(t, w), "(used 3, requested 3)") {
				t.Errorf("message %q", errorMessage(t, w))
			}

			// A layered answer: n 1 reserved, three layers settled.
			if w := do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox","n":1,"layers":3}`, "layered")); w.Code != http.StatusOK ||
				len(answeredImages(t, w.Body.Bytes())) != 3 {
				t.Fatalf("a layered answer under 2 left: %d %.200s, want 200 and three images", w.Code, w.Body.String())
			}
			if used := counterUsed(t, g, "research", typ); used != 6 {
				t.Errorf("window holds %d images, want 6: the layered answer's 3, past the limit", used)
			}
			if rec := g.usage.all(); len(rec) != 2 || rec[1].Units[config.UnitImages] != 3 {
				t.Errorf("records %+v, want the layered answer's settled at 3 images", rec)
			}
			expectError(t, do(t, g.h, imagesCall(`{"model":"painter","prompt":"a fox"}`, "full")), http.StatusTooManyRequests,
				"rate_limit_exceeded")

			for _, c := range []call{{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody},
				speechCall(`{"model":"tts","input":"hi"}`, "speech")} {
				if w := do(t, g.h, c); w.Code != http.StatusOK {
					t.Errorf("%s refused by the image limit: %d %s", c.path, w.Code, w.Body.String())
				}
			}
			if used := counterUsed(t, g, "research", typ); used != 6 {
				t.Errorf("window holds %d images after chat and speech, want 6", used)
			}
		})
	}
}

// An image answer cut short after its first bytes settles the larger of the images
// relayed and the n asked for — they were generated before its first byte — flagged
// partial; the client's connection is cut, never ended cleanly — before its headers,
// when the gateway had not flushed them yet (docs/specs/GATEWAY.md, Accounting →
// Partial; Providers: upstream failures).
func TestImagesCutShort(t *testing.T) {
	for _, c := range []struct {
		name, body string
		at         int
		images     int64
	}{
		{"within the first of three", `{"model":"painter","prompt":"a fox","n":3}`, 0, 3},
		{"within the fifth of six layers", `{"model":"painter","prompt":"a fox","n":2,"layers":3}`, 4, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g)
			g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: c.at, Kind: fakebackend.Cut}})
			req, err := http.NewRequestWithContext(context.Background(), "POST", serveGateway(t, g)+"/v1/images/generations",
				strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+workloadKey)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || err == nil {
					t.Errorf("status %d, read error %v: want the answer cut short", resp.StatusCode, err)
				}
			}
			rec := settledRecord(t, g)
			estimate := accounting.EstimateInput(provider.ImagesGenerations, []byte(c.body)).Total
			expectUnits(t, rec, imageUnits(estimate, c.images), true, true)
		})
	}
}

// withImageServers adds a second vllm-omni backend, "omni2" on other, and three
// models to withSpeechModels': stale-painter, whose one deployment names a model its
// server does not run once g's backend serves Qwen/Qwen-Image alone; drawer, that
// stale deployment beside omni2/Qwen/Qwen-Image; voice-or-image, a speech model's
// deployment (no diffusion stage) beside omni2/Qwen/Qwen-Image. The circuit opens
// after two failures.
func withImageServers(t *testing.T, g *testGateway, other *fakebackend.Backend) {
	t.Helper()
	g.backend.SetImageServedModel("Qwen/Qwen-Image")
	g.backend.SetNonImageModels("Qwen/Qwen3-TTS")
	withSpeechModels(t, g, withGlobal(t, `"circuit": { "failure_threshold": 2, "probe_interval_ms": 3600000 }`),
		func(doc string) string {
			doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "omni2": { "type": "vllm-omni", "base_url": "`+other.URL()+`/v1" },`)
			model := func(name string, deployments ...string) string {
				var list []string
				for _, d := range deployments {
					backend, backendModel, _ := strings.Cut(d, "/")
					list = append(list, `{ "backend": "`+backend+`", "model": "`+backendModel+`" }`)
				}
				return `"` + name + `": { "deployments": [` + strings.Join(list, ", ") + `],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } } },
    `
			}
			return replaceOnce(t, doc, `"painter": {`, model("stale-painter", "omni/Qwen/Qwen-Image-Old")+
				model("drawer", "omni/Qwen/Qwen-Image-Old", "omni2/Qwen/Qwen-Image")+
				model("voice-or-image", "omni/Qwen/Qwen3-TTS", "omni2/Qwen/Qwen-Image")+`"painter": {`)
		})
}

// vLLM-Omni's image 400 "Model mismatch" naming the deployment's model is the model
// missing there: 502 upstream_model_missing — the backend's text not relayed —
// retried on another deployment, a circuit failure, no images settled
// (docs/specs/GATEWAY.md, Providers → Wrong model on a host).
func TestImagesModelMismatch(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withImageServers(t, g, other)

	w := do(t, g.h, imagesCall(`{"model":"stale-painter","prompt":"a fox"}`, "stale"))
	expectError(t, w, http.StatusBadGateway, "upstream_model_missing")
	if strings.Contains(w.Body.String(), "mismatch") || strings.Contains(w.Body.String(), "Qwen") {
		t.Errorf("the backend's answer reached the client: %s", w.Body.String())
	}
	expectUnits(t, onlyRecord(t, g), imageUnits(0, 0), false, true)
	do(t, g.h, imagesCall(`{"model":"stale-painter","prompt":"a fox"}`, "stale-2"))
	if !circuitOpen(g, "omni", "Qwen/Qwen-Image-Old") {
		t.Error("circuit of omni/Qwen/Qwen-Image-Old closed after two mismatches, want open: a failure")
	}

	// drawer: a request meeting the stale deployment first is retried on the other —
	// until two mismatches open the stale one's circuit — and every request is
	// answered.
	g = newTestGateway(t)
	withImageServers(t, g, other)
	var lines string
	for _, id := range []string{"drawer-1", "drawer-2", "drawer-3", "drawer-4"} {
		if w := do(t, g.h, imagesCall(`{"model":"drawer","prompt":"a fox"}`, id)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
		lines += logLine(t, g, id) + "\n"
	}
	retried := strings.Count(lines, `"kaiak.tried":"omni/Qwen/Qwen-Image-Old:upstream_model_missing,omni2/Qwen/Qwen-Image:200"`)
	if retried == 0 || !circuitOpen(g, "omni", "Qwen/Qwen-Image-Old") && retried != 4 {
		t.Errorf("%d requests retried after the mismatch, the stale circuit open %v:\n%s", retried,
			circuitOpen(g, "omni", "Qwen/Qwen-Image-Old"), lines)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_retries_total{gen_ai_request_model="drawer",kaiak_backend_id="omni",kaiak_attempt_outcome="model_missing"} `+strconv.Itoa(retried))
}

// vLLM-Omni's image 503 "No diffusion stage" — an image request to a speech model —
// is the endpoint missing from the deployment's server: 502 upstream_endpoint_missing
// with no other deployment, the backend's text not relayed; neutral for the circuit;
// the deployment remembered and left out of image routing, the request retried on the
// model's other deployment (docs/specs/GATEWAY.md, Providers → An endpoint missing
// from a server).
func TestImagesNoDiffusionStage(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withImageServers(t, g, other)

	for range 3 {
		w := do(t, g.h, imagesCall(`{"model":"tts","prompt":"a fox"}`, "to-tts"))
		expectError(t, w, http.StatusBadGateway, "upstream_endpoint_missing")
		if strings.Contains(w.Body.String(), "diffusion") {
			t.Errorf("the backend's answer reached the client: %s", w.Body.String())
		}
	}
	if circuitOpen(g, "omni", "Qwen/Qwen3-TTS") {
		t.Error("circuit of omni/Qwen/Qwen3-TTS open, want closed: the outcome is neutral")
	}
	if got := rememberedDeployments(g.missing); !slices.Equal(got, []string{"omni/Qwen/Qwen3-TTS"}) {
		t.Errorf("remembered %v, want omni/Qwen/Qwen3-TTS", got)
	}
	if lines := warnings(g); len(lines) != 1 || !strings.Contains(lines[0], `"kaiak.endpoint":"images_generations"`) {
		t.Errorf("endpoint-missing warnings %v, want one naming images_generations", lines)
	}
	if w := do(t, g.h, speechCall(`{"model":"tts","input":"hi"}`, "speech")); w.Code != http.StatusOK {
		t.Errorf("speech to the remembered deployment: %d %s", w.Code, w.Body.String())
	}

	g = newTestGateway(t)
	withImageServers(t, g, other)
	before := len(other.Requests())
	for _, id := range []string{"mixed-1", "mixed-2", "mixed-3"} {
		if w := do(t, g.h, imagesCall(`{"model":"voice-or-image","prompt":"a fox"}`, id)); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	if n, m := len(g.backend.Requests()), len(other.Requests())-before; n != 1 || m != 3 {
		t.Errorf("the speech deployment got %d image requests, the image one %d; want 1 and 3: once remembered, it is left out", n, m)
	}
	expectMetricLines(t, scrape(g),
		`kaiak_retries_total{gen_ai_request_model="voice-or-image",kaiak_backend_id="omni",kaiak_attempt_outcome="endpoint_missing"} 1`)
}

// Nothing of the prompt, the negative prompt, the images answered, a LoRA path or the
// backend's error text reaches the log (docs/specs/GATEWAY.md, Observability: logs).
func TestImagesLogsHoldNoContent(t *testing.T) {
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g := newTestGateway(t)
	withImageServers(t, g, other)
	body := `{"model":"painter","prompt":"zanzibar quokka","negative_prompt":"whisper marmalade","n":2}`
	if w := do(t, g.h, imagesCall(body, "secret-1")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "iVBORw0KGgo") {
		t.Fatalf("%d %.200s", w.Code, w.Body.String())
	}
	do(t, g.h, imagesCall(strings.Replace(body, `"n":2`, `"lora":{"name":"s","path":"/srv/pomegranate"}`, 1), "secret-2"))
	do(t, g.h, imagesCall(strings.Replace(body, `"painter"`, `"stale-painter"`, 1), "secret-3"))
	do(t, g.h, imagesCall(strings.Replace(body, `"painter"`, `"tts"`, 1), "secret-4"))
	logs := g.logText()
	for _, secret := range []string{"zanzibar", "quokka", "marmalade", "pomegranate", "iVBORw0KGgo", "mismatch", "diffusion"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}
