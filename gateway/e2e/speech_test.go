package e2e

// Speech end to end (docs/specs/GATEWAY.md, Client API → owned fields: speech, speech
// batch; Providers → media answers; Accounting → Media usage; Limits → Check and
// reserve): one gateway passes speech to a vllm-omni backend — the fake backend
// playing vLLM-Omni — in each mode: the whole clip, its tokens from the response
// headers; the raw-audio stream, its tokens estimated and flagged; the SSE stream, its
// usage from speech.audio.done; the batch, its items' usage summed. Each answer comes
// back as the backend sent it, and each request settles with the seconds of audio
// relayed, rounded up once, under the operation generate_content and the output type
// speech — on the request line and in the usage metrics. A client gone mid-clip
// settles the seconds it was sent; a spent audio limit refuses speech alone; a model on
// vllm-omni serves no chat or embeddings; what the gateway refuses never reaches the
// backend, and nothing of the text, the audio or a backend's error text reaches the
// log.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// The backend-side models of the media scenarios.
const (
	omniSpeechModel = "Qwen/Qwen3-TTS"
	omniImageModel  = "Qwen/Qwen-Image"
)

// A media limit counts every request of its scope that adds to its measure, so each
// limited group of the media scenarios has a key of its own: audioKey's group allows
// 5 seconds of audio generated a minute, imagesKey's 5 images a minute.
var (
	audioKey, audioHash   = newKey()
	imagesKey, imagesHash = newKey()
)

// omniConfig is a config with a vllm-omni backend "omni" and an openai-compatible
// backend "oc", both on the fake backend at fakeURL; the models "tts" (on omni as
// omniSpeechModel; output limit default 300, ceiling 1000; $1 and $2 per million input
// and output tokens), "painter" (on omni as omniImageModel) and "chat" (on oc);
// global.max_images_per_request 4 and global.max_speech_batch_items 3; the group w with
// the key k of hash, the group audio (audio_seconds_out_per_minute 5) with audioKey and
// the group images (images_per_minute 5) with imagesKey — each allowed every model.
func omniConfig(fakeURL, hash string) map[string]any {
	model := func(backend, deployed string) map[string]any {
		return map[string]any{
			"deployments": []any{map[string]any{"backend": backend, "model": deployed}},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": true, "tools": false, "vision": false, "reasoning": false}},
		}
	}
	tts := model("omni", omniSpeechModel)
	tts["output_limit"] = map[string]any{"default": 300, "ceiling": 1000}
	tts["prices"] = []any{map[string]any{"effective_from": "2026-01-01", "tiers": []any{map[string]any{
		"above_input_tokens": 0, "usd_per_million": map[string]any{"tokens_in": 1, "tokens_out": 2}}}}}
	limited := func(typ string) map[string]any {
		return map[string]any{"allowed_models": []any{"*"}, "limits": []any{map[string]any{"type": typ, "value": 5}}}
	}
	return map[string]any{
		"format_version": 6,
		"global":         map[string]any{"max_images_per_request": 4, "max_speech_batch_items": 3},
		"backends": map[string]any{
			"omni": map[string]any{"type": "vllm-omni", "base_url": fakeURL + "/v1"},
			"oc":   backendEntry("openai-compatible", fakeURL),
		},
		"models": map[string]any{"tts": tts, "painter": model("omni", omniImageModel), "chat": model("oc", backendChatModel)},
		"groups": map[string]any{
			"w":      map[string]any{"allowed_models": []any{"*"}},
			"audio":  limited("audio_seconds_out_per_minute"),
			"images": limited("images_per_minute"),
		},
		"keys": map[string]any{
			"k":        map[string]any{"hash": hash, "group": "w"},
			"k-audio":  map[string]any{"hash": audioHash, "group": "audio"},
			"k-images": map[string]any{"hash": imagesHash, "group": "images"},
		},
	}
}

// startOmni starts a fake backend serving the media scenarios' models and a gateway on
// omniConfig; it returns the gateway, the fake backend and the key of group w. Both
// are stopped at test cleanup.
func startOmni(t *testing.T) (*gateway, *fakebackend.Backend, string) {
	t.Helper()
	fake := fakebackend.New()
	t.Cleanup(fake.Close)
	fake.SetModels(omniSpeechModel, omniImageModel, backendChatModel)
	key, hash := newKey()
	configFile := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, configFile, omniConfig(fake.URL(), hash))
	return startGateway(t, configFile), fake, key
}

// wavClip is a WAV answer of the fake backend: its header — with its sizes, or
// streamed — then dataBytes of silence.
func wavClip(rate, dataBytes int, streamed bool) []byte {
	return append(fakebackend.WAVHeader(rate, dataBytes, streamed), make([]byte, dataBytes)...)
}

// speechEvent is a speech SSE event's data as the client gets it.
type speechEvent struct {
	Type  string `json:"type"`
	Audio string `json:"audio"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// readSpeechEvents reads a speech SSE stream as the client got it: each event named
// by an event line its data's type repeats, every event but the last a delta, the last
// speech.audio.done. It returns the deltas' audio, decoded and joined, and the done
// event.
func readSpeechEvents(t *testing.T, body []byte) ([]byte, speechEvent) {
	t.Helper()
	payloads := events(body)
	if len(payloads) < 2 {
		t.Fatalf("speech stream %.300s: want deltas and done", body)
	}
	var audio []byte
	var ev speechEvent
	for i, payload := range payloads {
		ev = speechEvent{}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("event %s: %v", payload, err)
		}
		want := "speech.audio.delta"
		if i == len(payloads)-1 {
			want = "speech.audio.done"
		}
		if ev.Type != want {
			t.Fatalf("event %d is %s, want %s", i, ev.Type, want)
		}
		decoded, err := base64.StdEncoding.DecodeString(ev.Audio)
		if err != nil {
			t.Fatalf("delta %d's audio: %v", i, err)
		}
		audio = append(audio, decoded...)
	}
	if n := strings.Count(string(body), "event: speech.audio.delta\n"); n != len(payloads)-1 {
		t.Errorf("%d event lines naming a delta, want %d", n, len(payloads)-1)
	}
	return audio, ev
}

// wantLine checks a request's log line against want, field by field.
func wantLine(t *testing.T, line map[string]any, want map[string]any) {
	t.Helper()
	for field, value := range want {
		if line[field] != value {
			t.Errorf("log line %s = %v, want %v", field, line[field], value)
		}
	}
}

// costUSD is the request line's kaiak.usage.cost_usd of tokens in and out on "tts":
// $1 and $2 per million.
func costUSD(in, out int64) float64 { return float64(in*1000+out*2000) / 1e9 }

func TestSpeech(t *testing.T) {
	g, fake, key := startOmni(t)
	const timing = `[{"word":"Hello","start_time":0,"end_time":0.4}]`

	// 2.5 s of 16-bit mono audio (the fake's default) in each mode: 120 000 bytes at
	// Qwen3-TTS's 24 kHz, 40 000 at 8 kHz, 80 000 at 16 kHz — 3 s once rounded up. The
	// backend gets the request with the model edit and the default output limit under
	// max_new_tokens; the client gets the audio byte for byte, or the events, with the
	// word timing and never the usage headers; the whole clip and the SSE stream settle
	// the reported 7 tokens in and 4 out, the raw-audio stream the input estimate,
	// flagged.
	for _, c := range []struct {
		name, body, contentType string
		audio                   []byte // the body, or the SSE deltas' audio joined
		stream, sse, estimated  bool
	}{
		{"whole clip, wav", `{"model":"tts","input":"Hello there","voice":"vivian"}`, "audio/wav",
			wavClip(24000, 120000, false), false, false, false},
		{"whole clip, pcm", `{"model":"tts","input":"Hello there","response_format":"pcm","sample_rate":8000}`, "audio/pcm",
			make([]byte, 40000), false, false, false},
		{"raw-audio stream, wav", `{"model":"tts","input":"Hello there","stream_format":"audio"}`, "audio/wav",
			wavClip(24000, 120000, true), true, false, true},
		{"raw-audio stream, pcm", `{"model":"tts","input":"Hello there","stream_format":"audio","response_format":"pcm",` +
			`"sample_rate":8000}`, "audio/pcm", make([]byte, 40000), true, false, true},
		{"SSE stream, wav", `{"model":"tts","input":"Hello there","stream":true}`, "text/event-stream",
			wavClip(24000, 120000, true), true, true, false},
		{"SSE stream, pcm", `{"model":"tts","input":"Hello there","stream_format":"sse","response_format":"pcm",` +
			`"sample_rate":16000}`, "text/event-stream", make([]byte, 80000), true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake.SetReply(fakebackend.Reply{Header: map[string]string{"X-Word-Timestamps": timing}})
			defer fake.SetReply(fakebackend.Reply{})
			id := "speech-" + strings.NewReplacer(" ", "-", ",", "").Replace(c.name)
			before := len(fake.Requests())
			r := g.post(t, "/v1/audio/speech", key, id, json.RawMessage(c.body))
			if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != c.contentType {
				t.Fatalf("%d (%s) %.300s, want 200 in %s", r.StatusCode, r.Header.Get("Content-Type"), r.body, c.contentType)
			}
			audio := r.body
			if c.sse {
				var done speechEvent
				audio, done = readSpeechEvents(t, r.body)
				if done.Usage == nil || done.Usage.InputTokens != 7 || done.Usage.OutputTokens != 4 {
					t.Errorf("done %+v, want the backend's usage relayed", done)
				}
			}
			if !bytes.Equal(audio, c.audio) {
				t.Errorf("%d bytes of audio, want the backend's %d byte for byte", len(audio), len(c.audio))
			}
			if r.Header.Get("X-Word-Timestamps") != timing || r.Header.Get("X-Accel-Buffering") != "no" {
				t.Errorf("headers %v: want the word timing relayed and X-Accel-Buffering: no", r.Header)
			}
			for name := range r.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-vllm-omni") {
					t.Errorf("the client got the backend's %s", name)
				}
			}

			reqs := fake.Requests()
			if len(reqs) != before+1 {
				t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
			}
			want := strings.Replace(c.body, `"model":"tts"`, `"model":"`+omniSpeechModel+`"`, 1)
			want = strings.TrimSuffix(want, "}") + `,"max_new_tokens":300}`
			if got := reqs[len(reqs)-1]; got.Path != "/v1/audio/speech" || string(got.Body) != want {
				t.Errorf("backend got %s\n %s\nwant /v1/audio/speech\n %s", got.Path, got.Body, want)
			}

			in, out := int64(7), int64(4)
			if c.estimated {
				in, out = accounting.EstimateInput(provider.AudioSpeech, []byte(c.body)).Total, 0
			}
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.backend.id": "omni", "kaiak.backend.type": "vllm-omni",
				"gen_ai.operation.name": "generate_content", "gen_ai.output.type": "speech", "gen_ai.request.stream": c.stream,
				"gen_ai.usage.input_tokens": float64(in), "gen_ai.usage.output_tokens": float64(out),
				"kaiak.usage.audio.output.duration": 3.0, "kaiak.usage.cost_usd": costUSD(in, out),
				"kaiak.usage.estimated": c.estimated, "kaiak.usage.partial": false})
		})
	}

	// Three items of 1.25 s, the second errored: the answer relayed as sent, each
	// result's audio in its item's format; the usage of the two successful items
	// summed, their audio summed before one rounding (2.5 s → 3 s).
	t.Run("batch", func(t *testing.T) {
		fake.SetReply(fakebackend.Reply{AudioMillis: 1250, FailItems: []int{1}})
		defer fake.SetReply(fakebackend.Reply{})
		body := `{"model":"tts","voice":"vivian","items":[{"input":"one"},{"input":"two"},` +
			`{"input":"three","response_format":"pcm","sample_rate":8000}]}`
		before := len(fake.Requests())
		r := g.post(t, "/v1/audio/speech/batch", key, "speech-batch", json.RawMessage(body))
		if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("%d (%s) %.300s, want 200 with a JSON body", r.StatusCode, r.Header.Get("Content-Type"), r.body)
		}
		var answer struct {
			Results []struct {
				Index     int    `json:"index"`
				Status    string `json:"status"`
				AudioData string `json:"audio_data"`
				MediaType string `json:"media_type"`
				Error     string `json:"error"`
			} `json:"results"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		}
		if err := json.Unmarshal(r.body, &answer); err != nil || len(answer.Results) != 3 || answer.Succeeded != 2 || answer.Failed != 1 {
			t.Fatalf("answer %.300s (%v): want three results, two successful", r.body, err)
		}
		for i, want := range []struct {
			status, mediaType string
			audio             []byte
		}{
			{"success", "audio/wav", wavClip(24000, 60000, false)},
			{"error", "", nil},
			{"success", "audio/pcm", make([]byte, 20000)},
		} {
			got := answer.Results[i]
			audio, err := base64.StdEncoding.DecodeString(got.AudioData)
			if got.Index != i || got.Status != want.status || got.MediaType != want.mediaType || err != nil || !bytes.Equal(audio, want.audio) {
				t.Errorf("result %d: index %d, %s, %s, %d bytes of audio (%v); want %s, %s, %d bytes as the backend sent them",
					i, got.Index, got.Status, got.MediaType, len(audio), err, want.status, want.mediaType, len(want.audio))
			}
		}
		if answer.Results[1].Error != fakebackend.SpeechBatchItemError {
			t.Errorf("the errored item's error %q, want the backend's relayed", answer.Results[1].Error)
		}

		reqs := fake.Requests()
		if len(reqs) != before+1 {
			t.Fatalf("backend got %d requests, want 1", len(reqs)-before)
		}
		want := strings.Replace(body, `"model":"tts"`, `"model":"`+omniSpeechModel+`"`, 1)
		want = strings.TrimSuffix(want, "}") + `,"max_new_tokens":300}`
		if got := reqs[len(reqs)-1]; got.Path != "/v1/audio/speech/batch" || string(got.Body) != want {
			t.Errorf("backend got %s\n %s\nwant /v1/audio/speech/batch\n %s", got.Path, got.Body, want)
		}
		wantLine(t, g.settled(t, "speech-batch"), map[string]any{"kaiak.backend.id": "omni", "kaiak.backend.type": "vllm-omni",
			"gen_ai.operation.name": "generate_content", "gen_ai.output.type": "speech", "gen_ai.request.stream": false,
			"gen_ai.usage.input_tokens": 14.0, "gen_ai.usage.output_tokens": 8.0, "kaiak.usage.audio.output.duration": 3.0,
			"kaiak.usage.cost_usd": costUSD(14, 8), "kaiak.usage.estimated": false, "kaiak.usage.partial": false})
	})

	// A client that reads two of the raw-audio stream's four pieces of 0.625 s (10 000
	// bytes at 8 kHz) and leaves: the backend's request is cancelled, and the request
	// settles the 1.25 s it was sent, rounded up, flagged partial (Accounting →
	// Partial).
	t.Run("a client gone mid-clip", func(t *testing.T) {
		pace := make(chan struct{})
		fake.SetReply(fakebackend.Reply{Pace: pace})
		defer fake.SetReply(fakebackend.Reply{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		body := `{"model":"tts","input":"Hello there","stream_format":"audio","response_format":"pcm","sample_rate":8000}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.api+"/v1/audio/speech", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Request-Id", "speech-gone")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%d, want 200", resp.StatusCode)
		}
		reqs := fake.Requests()
		upstream := reqs[len(reqs)-1] // recorded before the fake answered
		piece := make([]byte, 10000)
		if _, err := io.ReadFull(resp.Body, piece); err != nil {
			t.Fatalf("the first piece: %v", err)
		}
		select {
		case pace <- struct{}{}:
		case <-time.After(waitLimit):
			t.Fatal("the backend never asked for its second piece")
		}
		if _, err := io.ReadFull(resp.Body, piece); err != nil {
			t.Fatalf("the second piece: %v", err)
		}
		cancel()

		select {
		case <-upstream.Canceled():
		case <-time.After(waitLimit):
			t.Fatal("the backend's request was not cancelled")
		}
		wantLine(t, g.settled(t, "speech-gone"), map[string]any{"gen_ai.output.type": "speech", "gen_ai.request.stream": true,
			"kaiak.usage.audio.output.duration": 2.0, "kaiak.usage.estimated": true, "kaiak.usage.partial": true,
			"kaiak.relay_end": "client_closed"})
	})

	// Two clips of 3 s take the group's 5 seconds a minute past its value — nothing is
	// reserved, the seconds settle after — and the next speech request is refused,
	// 429 audio_seconds, with no rate-limit headers; chat and image generation in the
	// same group are not.
	t.Run("audio generated per minute", func(t *testing.T) {
		speech := json.RawMessage(`{"model":"tts","input":"hi"}`)
		for i := range 2 {
			id := fmt.Sprintf("audio-limit-%d", i)
			if r := g.post(t, "/v1/audio/speech", audioKey, id, speech); r.StatusCode != http.StatusOK {
				t.Fatalf("speech %d: %d %s, want 200", i+1, r.StatusCode, r.body)
			}
			g.settled(t, id)
		}
		r := g.post(t, "/v1/audio/speech", audioKey, "audio-limit-refused", speech)
		e, _ := r.json(t)["error"].(map[string]any)
		if r.StatusCode != http.StatusTooManyRequests || e["code"] != "rate_limit_exceeded" || e["type"] != "audio_seconds" ||
			r.Header.Get("Retry-After") == "" {
			t.Fatalf("%d %s (Retry-After %q), want 429 audio_seconds with a wait", r.StatusCode, r.body, r.Header.Get("Retry-After"))
		}
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-ratelimit-") {
				t.Errorf("an audio limit's refusal sent %s", name)
			}
		}
		wantLine(t, g.settled(t, "audio-limit-refused"), map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.group": "audio",
			"kaiak.limit.type": "audio_seconds_out_per_minute", "kaiak.limit.configured": 5.0})
		if got := g.metric(t, `kaiak_limit_rejections_total{kaiak_limit_scope="group",kaiak_limit_type="audio_seconds_out_per_minute"}`); got != 1 {
			t.Errorf("audio limit rejections = %v, want 1", got)
		}
		if r := g.post(t, "/v1/chat/completions", audioKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Errorf("chat in the group: %d %s, want 200", r.StatusCode, r.body)
		}
		if r := g.post(t, "/v1/images/generations", audioKey, "", json.RawMessage(`{"model":"painter","prompt":"a fox"}`)); r.StatusCode != http.StatusOK {
			t.Errorf("an image in the group: %d %.300s, want 200", r.StatusCode, r.body)
		}
	})

	// A model on vllm-omni lists the media endpoints, and no other (Client API → model
	// entry).
	t.Run("the model entry lists the media endpoints", func(t *testing.T) {
		status, body := g.get(t, "/v1/models/tts", key)
		var entry struct {
			Endpoints []string `json:"endpoints"`
		}
		if err := json.Unmarshal(body, &entry); status != http.StatusOK || err != nil ||
			strings.Join(entry.Endpoints, ",") != "audio_speech,audio_speech_batch,images_generations" {
			t.Errorf("/v1/models/tts = %d %s, want the three media endpoints", status, body)
		}
	})

	// A model whose deployments are all vllm-omni serves no chat or embeddings — its
	// server's chat would generate media no media limit sees — and what the media
	// endpoints refuse, the caps included, is refused before any backend, in OpenAI's
	// shape, naming the member at fault (Client API → errors).
	t.Run("refused before any backend", func(t *testing.T) {
		before := len(fake.Requests())
		for _, c := range []struct {
			name, path string
			body       any
			code       string
			param      any // nil: none
		}{
			{"chat to a speech model", "/v1/chat/completions", chatBody("tts", false, nil), "endpoint_not_served", nil},
			{"embeddings to an image model", "/v1/embeddings", map[string]any{"model": "painter", "input": "hello"},
				"endpoint_not_served", nil},
			{"speech to a chat model", "/v1/audio/speech", json.RawMessage(`{"model":"chat","input":"hi"}`), "endpoint_not_served", nil},
			{"mp3", "/v1/audio/speech", json.RawMessage(`{"model":"tts","input":"hi","response_format":"mp3"}`),
				"response_format_unsupported", "response_format"},
			{"pcm without its rate", "/v1/audio/speech", json.RawMessage(`{"model":"tts","input":"hi","response_format":"pcm"}`),
				"missing_required_parameter", "sample_rate"},
			{"a reference audio by URL", "/v1/audio/speech",
				json.RawMessage(`{"model":"tts","input":"hi","ref_audio":"https://example.com/voice.wav"}`),
				"external_reference_unsupported", "ref_audio"},
			{"more items than the cap", "/v1/audio/speech/batch",
				json.RawMessage(`{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c"},{"input":"d"}]}`),
				"invalid_value", "items"},
			{"an item's format", "/v1/audio/speech/batch",
				json.RawMessage(`{"model":"tts","items":[{"input":"a"},{"input":"b","response_format":"mp3"}]}`),
				"response_format_unsupported", "items[1].response_format"},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := g.post(t, c.path, key, "", c.body)
				e, _ := r.json(t)["error"].(map[string]any)
				if r.StatusCode != http.StatusBadRequest || e["code"] != c.code || e["param"] != c.param {
					t.Errorf("%d %s, want 400 %s on %v", r.StatusCode, r.body, c.code, c.param)
				}
				if c.code == "endpoint_not_served" && !strings.Contains(fmt.Sprint(e["message"]), c.path) {
					t.Errorf("message %q does not name %s", e["message"], c.path)
				}
			})
		}
		if n := len(fake.Requests()) - before; n != 0 {
			t.Errorf("the backend got %d requests for refused ones", n)
		}
	})

	// The usage metrics count the seconds of audio generated per key and model, under
	// generate_content and speech: six clips and the batch of 3 s each, complete; the
	// client gone's 2 s, partial; the audio group's two clips.
	usage := func(key, group, status string) string {
		return `{kaiak_key_group="` + group + `",kaiak_key_root_group="` + group + `",kaiak_key_id="` + key +
			`",gen_ai_request_model="tts",gen_ai_operation_name="generate_content",gen_ai_output_type="speech",kaiak_usage_status="` +
			status + `"}`
	}
	for series, want := range map[string]float64{
		`kaiak_usage_audio_output_duration_seconds_total` + usage("k", "w", "complete"):           21,
		`kaiak_usage_audio_output_duration_seconds_total` + usage("k", "w", "partial"):            2,
		`kaiak_usage_audio_output_duration_seconds_total` + usage("k-audio", "audio", "complete"): 6,
		`kaiak_usage_records_total` + usage("k", "w", "complete"):                                 7,
	} {
		if got := g.metric(t, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	// Nothing of the text, the audio ("UklGR": a WAV header's "RIFF" in base64), the
	// word timing or an item's error text reaches the log.
	for _, content := range []string{"Hello there", "UklGR", fakebackend.SpeechBatchItemError, timing} {
		if strings.Contains(g.logs.text(), content) {
			t.Errorf("the log holds %q", content)
		}
	}
	g.stop(t)
}
