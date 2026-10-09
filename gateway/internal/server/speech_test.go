package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// withSpeechModels swaps in the test config with vllm-omni backends on the fake
// backend — "omni", and "omni-slow" whose response and stall timeouts are 150 ms and
// first-event timeout 5 s — and speech models: "tts" on omni (as "Qwen/Qwen3-TTS";
// output limit default 300, ceiling 1000; priced at $1 and $2 per million input and
// output tokens), "tts-free" there with no output limit and no price, "tts-slow" on
// omni-slow, and "painter" on omni (as "Qwen/Qwen-Image"), a model that does not
// speak; then edits applied.
func withSpeechModels(t *testing.T, g *testGateway, edits ...func(doc string) string) {
	t.Helper()
	g.backend.SetNonSpeechModels("Qwen/Qwen-Image")
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "omni": { "type": "vllm-omni", "base_url": "`+g.backend.URL()+`/v1" },
    "omni-slow": { "type": "vllm-omni", "base_url": "`+g.backend.URL()+`/v1", "response_timeout_ms": 150,
      "stall_timeout_ms": 150, "first_event_timeout_ms": 5000 },`)
		model := func(name, backend, backendModel, extra string) string {
			return `"` + name + `": { "deployments": [{ "backend": "` + backend + `", "model": "` + backendModel + `" }],
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": true, "tools": false, "vision": false, "reasoning": false } }` + extra + ` },
    `
		}
		priced := `, "output_limit": { "default": 300, "ceiling": 1000 },
      "prices": [{ "effective_from": "2020-01-01",
        "tiers": [{ "above_input_tokens": 0, "usd_per_million": { "tokens_in": 1, "tokens_out": 2 } }] }]`
		doc = replaceOnce(t, doc, `"models": { `, `"models": {
    `+model("tts", "omni", "Qwen/Qwen3-TTS", priced)+model("tts-free", "omni", "Qwen/Qwen3-TTS-free", "")+
			model("tts-slow", "omni-slow", "Qwen/Qwen3-TTS-slow", "")+model("painter", "omni", "Qwen/Qwen-Image", ""))
		for _, edit := range edits {
			doc = edit(doc)
		}
		return doc
	})
}

// speechEvents reads a speech SSE stream as the client got it: each event's name, the
// audio of its deltas decoded and joined, and its last event's data.
func speechEvents(t *testing.T, stream string) (names []string, audio []byte, last string) {
	t.Helper()
	for block := range strings.SplitSeq(strings.TrimSuffix(stream, "\n\n"), "\n\n") {
		name, data, ok := strings.Cut(block, "\ndata: ")
		if !ok || !strings.HasPrefix(name, "event: ") {
			t.Fatalf("block %q is no speech event", block)
		}
		names, last = append(names, strings.TrimPrefix(name, "event: ")), data
		var delta struct{ Audio string }
		if err := json.Unmarshal([]byte(data), &delta); err != nil {
			t.Fatalf("event data %s: %v", data, err)
		}
		decoded, err := base64.StdEncoding.DecodeString(delta.Audio)
		if err != nil {
			t.Fatalf("delta audio: %v", err)
		}
		audio = append(audio, decoded...)
	}
	return names, audio, last
}

// speechCall is a speech request of the eval group's key carrying body, request ID id.
func speechCall(body, id string) call {
	return call{method: "POST", path: "/v1/audio/speech", key: workloadKey, body: body,
		header: map[string]string{"X-Request-Id": id}}
}

// speechUsage is a speech record's units: its tokens and seconds of audio generated.
func speechUnits(in, out, seconds int64) accounting.Units {
	u := units(in, 0, 0, out, 0)
	u[config.UnitAudioSecondsOut] = seconds
	return u
}

// A speech request passes the whole pipeline to a vllm-omni deployment, in each mode
// and format: sent with the model edit alone — the model's default output limit is
// reserved, never sent — everything else as the client wrote it; answered as the
// backend answered — its audio byte for byte, or its events, its content type and
// word timing, never the usage headers — flushed and unbuffered; settled from the
// whole clip's headers or the SSE stream's speech.audio.done, or estimated on the
// raw-audio stream, with the audio read measured into audio_seconds_out (2.5 s → 3),
// priced on its tokens; operation generate_content and output type speech on the
// request line and the usage metrics, the two streams streams there and timed to
// their first audio (docs/specs/GATEWAY.md, Client API → owned fields: speech;
// Providers; Accounting → Media usage; Observability).
func TestSpeechThroughThePipeline(t *testing.T) {
	for _, c := range []struct {
		name, body, contentType string
		size                    int
		raw, sse                bool
	}{
		{"whole clip, wav", `{"model":"tts","input":"Hello there","voice":"vivian"}`, "audio/wav", 44 + 120000, false, false},
		{"whole clip, pcm", `{"model":"tts","input":"Hello there","response_format":"pcm","sample_rate":8000}`,
			"audio/pcm", 40000, false, false},
		{"raw-audio stream, wav", `{"model":"tts","input":"Hello there","stream_format":"audio"}`, "audio/wav", 44 + 120000, true, false},
		{"raw-audio stream, pcm", `{"model":"tts","input":"Hello there","stream_format":"audio","stream":true,` +
			`"response_format":"pcm","sample_rate":8000}`, "audio/pcm", 40000, true, false},
		{"SSE stream, wav", `{"model":"tts","input":"Hello there","stream":true}`, "text/event-stream", 44 + 120000, false, true},
		{"SSE stream, pcm", `{"model":"tts","input":"Hello there","stream_format":"sse","response_format":"pcm","sample_rate":8000}`,
			"text/event-stream", 40000, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g)
			g.backend.SetReply(fakebackend.Reply{Header: map[string]string{"X-Word-Timestamps": `[{"word":"Hello"}]`}})
			w := do(t, g.h, speechCall(c.body, "sp-1"))
			audio := w.Body.Bytes()
			if c.sse && w.Code == http.StatusOK {
				var names []string
				names, audio, _ = speechEvents(t, w.Body.String())
				if names[0] != "speech.audio.delta" || names[len(names)-1] != "speech.audio.done" {
					t.Errorf("events %v: want deltas, then done", names)
				}
			}
			if w.Code != http.StatusOK || len(audio) != c.size || w.Header().Get("Content-Type") != c.contentType {
				t.Fatalf("%d, %d bytes of audio in %s: want 200, %d bytes in %s", w.Code, len(audio), w.Header().Get("Content-Type"),
					c.size, c.contentType)
			}
			if c.contentType == "audio/wav" || (c.sse && !strings.Contains(c.body, "pcm")) {
				if !bytes.HasPrefix(audio, []byte("RIFF")) {
					t.Errorf("audio not relayed as sent: % x…", audio[:min(len(audio), 48)])
				}
			}
			if w.Header().Get("X-Word-Timestamps") != `[{"word":"Hello"}]` || w.Header().Get("X-Accel-Buffering") != "no" ||
				!w.Flushed {
				t.Errorf("headers %v, flushed %v: want the word timing, X-Accel-Buffering: no, flushed", w.Header(), w.Flushed)
			}
			for name := range w.Header() {
				if strings.HasPrefix(name, "X-Vllm-Omni") {
					t.Errorf("the client got %s", name)
				}
			}

			reqs := g.backend.Requests()
			want := strings.Replace(c.body, `"model":"tts"`, `"model":"Qwen/Qwen3-TTS"`, 1)
			if len(reqs) != 1 || reqs[0].Path != "/v1/audio/speech" || string(reqs[0].Body) != want {
				t.Fatalf("backend requests %+v, want one to /v1/audio/speech with\n %s", reqs, want)
			}

			rec := onlyRecord(t, g)
			in, out := int64(7), int64(4)
			if c.raw {
				in, out = accounting.EstimateInput(provider.AudioSpeech, []byte(c.body)).Total, 0
			}
			expectUnits(t, rec, speechUnits(in, out, 3), c.raw, false)
			if rec.Operation != "generate_content" || rec.OutputType != "speech" || rec.CostNanoUSD != in*1000+out*2000 {
				t.Errorf("record operation %q, output type %q, cost %d; want generate_content, speech, %d", rec.Operation,
					rec.OutputType, rec.CostNanoUSD, in*1000+out*2000)
			}
			line := logLine(t, g, "sp-1")
			for _, want := range []string{`"gen_ai.operation.name":"generate_content"`, `"gen_ai.output.type":"speech"`,
				`"gen_ai.request.stream":` + strconv.FormatBool(c.raw || c.sse), `"kaiak.backend.type":"vllm-omni"`,
				`"kaiak.usage.audio.output.duration":3`, `"gen_ai.usage.input_tokens":` + strconv.FormatInt(in, 10)} {
				if !strings.Contains(line, want) {
					t.Errorf("log line lacks %s: %s", want, line)
				}
			}
			usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="tts",` +
				`gen_ai_operation_name="generate_content",gen_ai_output_type="speech",kaiak_usage_status="complete"}`
			text := g.metricsText()
			expectMetricLines(t, text,
				`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/audio/speech",`+
					`http_response_status_code="200",gen_ai_request_model="tts"} 1`,
				`kaiak_usage_records_total`+usage+` 1`,
				`kaiak_usage_audio_output_duration_seconds_total`+usage+` 3`)
			ttft := `kaiak_time_to_first_token_seconds_count{gen_ai_request_model="tts",kaiak_backend_id="omni"} 1`
			if strings.Contains(text, ttft) != (c.raw || c.sse) {
				t.Errorf("time to first token observed %v, want %v: only a stream is timed to its first audio",
					strings.Contains(text, ttft), c.raw || c.sse)
			}
		})
	}
}

// What the gateway refuses on /v1/audio/speech it refuses before routing, in
// OpenAI's error shape, naming the member at fault: audio it cannot measure, PCM
// without its rate, a reference audio the backend would fetch or read — every
// variant, each entry of a list, a references entry's audio_path — extra_params,
// whatever it holds (the model's adapter can fetch what a key names), owned fields of
// the wrong type or named twice, an output limit out of range or above the model's
// ceiling (the gateway sends no max_new_tokens of its own making), a model on no
// vllm-omni deployment (docs/specs/GATEWAY.md, Client API → owned fields: speech;
// errors).
func TestSpeechRefusals(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	const inline = `"data:audio/wav;base64,UklGRg=="`
	for _, c := range []struct {
		name, body, code, param string
	}{
		{"mp3", `{"model":"tts","input":"hi","response_format":"mp3"}`, "response_format_unsupported", "response_format"},
		{"flac", `{"model":"tts","input":"hi","response_format":"flac"}`, "response_format_unsupported", "response_format"},
		{"opus", `{"model":"tts","input":"hi","response_format":"opus"}`, "response_format_unsupported", "response_format"},
		{"WAV in capitals", `{"model":"tts","input":"hi","response_format":"WAV"}`, "response_format_unsupported", "response_format"},
		{"format not a string", `{"model":"tts","input":"hi","response_format":1}`, "invalid_type", "response_format"},
		{"pcm without a rate", `{"model":"tts","input":"hi","response_format":"pcm"}`, "missing_required_parameter", "sample_rate"},
		{"pcm with a null rate", `{"model":"tts","input":"hi","response_format":"pcm","sample_rate":null}`,
			"missing_required_parameter", "sample_rate"},
		{"rate not an integer", `{"model":"tts","input":"hi","response_format":"pcm","sample_rate":"8000"}`, "invalid_type", "sample_rate"},
		{"rate fractional", `{"model":"tts","input":"hi","sample_rate":8000.5}`, "invalid_type", "sample_rate"},
		{"ref_audio URL", `{"model":"tts","input":"hi","ref_audio":"https://example.com/voice.wav"}`,
			"external_reference_unsupported", "ref_audio"},
		{"ref_audio file URI", `{"model":"tts","input":"hi","ref_audio":"file:///srv/voices/a.wav"}`,
			"external_reference_unsupported", "ref_audio"},
		{"ref_audio path", `{"model":"tts","input":"hi","ref_audio":"/srv/voices/a.wav"}`, "external_reference_unsupported", "ref_audio"},
		{"ref_audio other scheme", `{"model":"tts","input":"hi","ref_audio":"s3://bucket/a.wav"}`,
			"external_reference_unsupported", "ref_audio"},
		{"ref_audio empty", `{"model":"tts","input":"hi","ref_audio":""}`, "external_reference_unsupported", "ref_audio"},
		{"ref_audio list entry", `{"model":"tts","input":"hi","ref_audio":[` + inline + `,"http://example.com/b.wav"]}`,
			"external_reference_unsupported", "ref_audio[1]"},
		{"ref_audio list entry not a string", `{"model":"tts","input":"hi","ref_audio":[` + inline + `,7]}`, "invalid_type", "ref_audio[1]"},
		{"ref_audio list entry null", `{"model":"tts","input":"hi","ref_audio":[null]}`, "invalid_type", "ref_audio[0]"},
		{"ref_audio an object", `{"model":"tts","input":"hi","ref_audio":{"url":"x"}}`, "invalid_type", "ref_audio"},
		{"ref_audio_2 URL", `{"model":"tts","input":"hi","ref_audio":` + inline + `,"ref_audio_2":"http://example.com/c.wav"}`,
			"external_reference_unsupported", "ref_audio_2"},
		{"ref_audio_2 a list", `{"model":"tts","input":"hi","ref_audio_2":[` + inline + `]}`, "invalid_type", "ref_audio_2"},
		{"references path", `{"model":"tts","input":"hi","references":[{"audio_path":` + inline + `},{"audio_path":"/srv/a.wav","text":"x"}]}`,
			"external_reference_unsupported", "references[1].audio_path"},
		{"references audio_path not a string", `{"model":"tts","input":"hi","references":[{"audio_path":1}]}`,
			"invalid_type", "references[0].audio_path"},
		{"references audio_path named twice", `{"model":"tts","input":"hi","references":[{"audio_path":` + inline +
			`,"audio_path":"/srv/a.wav"}]}`, "duplicate_member", "references[0].audio_path"},
		{"references not a list", `{"model":"tts","input":"hi","references":{"audio_path":"/srv/a.wav"}}`, "invalid_type", "references"},
		{"references entry not an object", `{"model":"tts","input":"hi","references":["/srv/a.wav"]}`, "invalid_type", "references[0]"},
		{"ref_audio named twice", `{"model":"tts","input":"hi","ref_audio":` + inline + `,"ref_audio":"/srv/a.wav"}`,
			"duplicate_member", "ref_audio"},
		{"stream not a boolean", `{"model":"tts","input":"hi","stream":"yes"}`, "invalid_type", "stream"},
		{"stream_format not a string", `{"model":"tts","input":"hi","stream_format":true}`, "invalid_type", "stream_format"},
		{"max_new_tokens not an integer", `{"model":"tts","input":"hi","max_new_tokens":"many"}`, "invalid_type", "max_new_tokens"},
		{"max_new_tokens negative", `{"model":"tts","input":"hi","max_new_tokens":-1}`, "invalid_value", "max_new_tokens"},
		{"max_new_tokens above the context", `{"model":"tts","input":"hi","max_new_tokens":8193}`, "invalid_value", "max_new_tokens"},
		{"max_new_tokens above the ceiling", `{"model":"tts","input":"hi","max_new_tokens":1001}`, "invalid_value", "max_new_tokens"},
		{"extra_params naming a URL", `{"model":"tts","input":"hi","extra_params":{"emo_audio":"http://169.254.169.254/latest/meta-data/x.wav"}}`,
			"external_reference_unsupported", "extra_params"},
		{"extra_params naming no URL", `{"model":"tts","input":"hi","extra_params":{"emo_alpha":0.5}}`,
			"external_reference_unsupported", "extra_params"},
		{"extra_params empty", `{"model":"tts","input":"hi","extra_params":{}}`, "external_reference_unsupported", "extra_params"},
		{"extra_params null", `{"model":"tts","input":"hi","extra_params":null}`, "external_reference_unsupported", "extra_params"},
		{"extra_params not an object", `{"model":"tts","input":"hi","extra_params":"x"}`, "external_reference_unsupported", "extra_params"},
		{"model missing", `{"input":"hi"}`, "missing_required_parameter", "model"},
		{"not on vllm-omni", `{"model":"open","input":"hi"}`, "endpoint_not_served", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, speechCall(c.body, "refused"))
			expectError(t, w, http.StatusBadRequest, c.code)
			if _, _, param := openAIError(t, w); (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q: %s", param, c.param, w.Body.String())
			}
			if msg := errorMessage(t, w); strings.Contains(msg, "example.com") || strings.Contains(msg, "/srv/") ||
				strings.Contains(msg, "169.254") || strings.Contains(msg, "emo_") {
				t.Errorf("message %q echoes the value", msg)
			}
			if c.param == "max_new_tokens" && strings.Contains(c.body, "1001") &&
				!strings.Contains(errorMessage(t, w), "at most 1000 tokens (its output-limit ceiling)") {
				t.Errorf("message %q does not name the ceiling", errorMessage(t, w))
			}
			if c.code == "endpoint_not_served" && !strings.Contains(errorMessage(t, w), "/v1/audio/speech") {
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
}

// What the gateway does not own passes untouched, and inline reference audio passes:
// a data URL in any case, as a string, every entry of a list, ref_audio_2 and
// references' audio_path, null values as absent; a stream_format the gateway does
// not know reaches the backend, timed as the whole clip, for the backend to refuse.
func TestSpeechPassesInlineReferencesAndWhatItDoesNotOwn(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	const inline = `"data:audio/wav;base64,UklGRg=="`
	for _, body := range []string{
		`{"model":"tts-free","input":"hi","ref_audio":` + inline + `,"ref_text":"hello","task_type":"Base"}`,
		`{"model":"tts-free","input":"hi","ref_audio":"DATA:audio/wav;base64,UklGRg=="}`,
		`{"model":"tts-free","input":"hi","ref_audio":[` + inline + `,` + inline + `],"ref_audio_2":` + inline + `}`,
		`{"model":"tts-free","input":"hi","references":[{"audio_path":` + inline + `,"text":"hello"},{"text":"no audio"}]}`,
		`{"model":"tts-free","input":"hi","ref_audio":null,"ref_audio_2":null,"references":null,"response_format":null,` +
			`"stream":null,"stream_format":null,"sample_rate":null}`,
		`{"model":"tts-free","input":"hi","response_format":"wav","sample_rate":16000,"speed":1.2,"seed":7,` +
			`"extra":{"model":"keep"},"word_timestamps":true,"stream":false}`,
	} {
		w := do(t, g.h, speechCall(body, "pass"))
		if w.Code != http.StatusOK {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
			continue
		}
		reqs := g.backend.Requests()
		if sent, want := string(reqs[len(reqs)-1].Body), strings.Replace(body, `"tts-free"`, `"Qwen/Qwen3-TTS-free"`, 1); sent != want {
			t.Errorf("sent\n %s\nwant\n %s", sent, want)
		}
	}

	w := do(t, g.h, speechCall(`{"model":"tts-free","input":"hi","stream_format":"chunks"}`, "unknown-format"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "stream_format must be") {
		t.Errorf("unknown stream_format: %d %s, want the backend's 400 relayed", w.Code, w.Body.String())
	}
	if line := logLine(t, g, "unknown-format"); !strings.Contains(line, `"gen_ai.request.stream":false`) {
		t.Errorf("unknown stream_format not timed as the whole clip: %s", line)
	}
}

// max_new_tokens is speech's output-limit key (docs/specs/GATEWAY.md, Limits →
// Output-limit keys), and the gateway sends none of its own making: a request naming
// none is sent without it — vLLM-Omni budgets it itself — and reserves the model's
// default; one within the ceiling, or at it, is kept (one above it is refused:
// TestSpeechRefusals); a model with no output limit sends what the client sent, and
// reserves no output when the client sent none. The reservation counts the effective
// limit.
func TestSpeechOutputLimit(t *testing.T) {
	for _, c := range []struct {
		body, want string
		reserved   int64
	}{
		{`{"model":"tts","input":"hi"}`, `{"model":"Qwen/Qwen3-TTS","input":"hi"}`, 300},
		{`{"model":"tts","input":"hi","max_new_tokens":1000}`, `{"model":"Qwen/Qwen3-TTS","input":"hi","max_new_tokens":1000}`, 1000},
		{`{"model":"tts","input":"hi","max_new_tokens":200}`, `{"model":"Qwen/Qwen3-TTS","input":"hi","max_new_tokens":200}`, 200},
		{`{"model":"tts","input":"hi","max_tokens":5000}`, `{"model":"Qwen/Qwen3-TTS","input":"hi","max_tokens":5000}`, 300},
		{`{"model":"tts-free","input":"hi"}`, `{"model":"Qwen/Qwen3-TTS-free","input":"hi"}`, 0},
		{`{"model":"tts-free","input":"hi","max_new_tokens":7000}`, `{"model":"Qwen/Qwen3-TTS-free","input":"hi","max_new_tokens":7000}`, 7000},
	} {
		g := newTestGateway(t)
		withSpeechModels(t, g, func(doc string) string {
			return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
		})
		w := do(t, g.h, speechCall(c.body, "limit"))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.body, w.Code, w.Body.String())
		}
		reqs := g.backend.Requests()
		if sent := string(reqs[len(reqs)-1].Body); sent != c.want {
			t.Errorf("%s sent\n %s\nwant\n %s", c.body, sent, c.want)
		}
		estimate := accounting.EstimateInput(provider.AudioSpeech, []byte(c.body)).Total
		if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.FormatInt(100000-estimate-c.reserved, 10) {
			t.Errorf("%s: remaining tokens at admission %s, want the reservation of %d + %d taken", c.body, got, estimate, c.reserved)
		}
	}

	// A token limit too small for the default reserved refuses the request as too
	// large, though the default is never sent.
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 200 }] }`)
	})
	w := do(t, g.h, speechCall(`{"model":"tts","input":"hi"}`, "too-large"))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(errorMessage(t, w), "Request too large") {
		t.Errorf("%d %s: want 429, too large for 200 tokens", w.Code, w.Body.String())
	}
}

// A whole clip whose usage headers are missing, or not both decimal integers, is
// estimated: the input estimate, no output, flagged — its audio still measured
// (docs/specs/GATEWAY.md, Accounting → Media usage, Estimation).
func TestSpeechUsageHeadersMissingOrMalformed(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply fakebackend.Reply
	}{
		{"missing", fakebackend.Reply{OmitUsage: true}},
		{"malformed", fakebackend.Reply{OmitUsage: true, Header: map[string]string{
			"X-Vllm-Omni-Input-Tokens": "12 tokens", "X-Vllm-Omni-Output-Tokens": "40"}}},
		{"one of the two", fakebackend.Reply{OmitUsage: true, Header: map[string]string{"X-Vllm-Omni-Output-Tokens": "40"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g)
			g.backend.SetReply(c.reply)
			body := `{"model":"tts","input":"Read this sentence aloud, please."}`
			if w := do(t, g.h, speechCall(body, "est")); w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			in := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
			expectUnits(t, onlyRecord(t, g), speechUnits(in, 0, 3), true, false)
			if line := logLine(t, g, "est"); !strings.Contains(line, `"kaiak.usage.estimated":true`) {
				t.Errorf("log line misses the flag: %s", line)
			}
		})
	}
}

// The input estimate of a speech request is its body by bytes, each inline reference
// audio a flat 1000 tokens (docs/specs/GATEWAY.md, Limits → The input estimate).
func TestSpeechInputEstimate(t *testing.T) {
	audio := `"data:audio/wav;base64,` + strings.Repeat("A", 600) + `"`
	body := `{"model":"tts","input":"hello","ref_audio":` + audio + `}`
	got := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
	if want := accounting.EstimateTokens(int64(len(body)-len(audio))) + accounting.InlineMediaTokens; got != want {
		t.Errorf("estimate %d, want %d: the text by bytes and the reference audio as one media item", got, want)
	}
}

// A client gone mid-clip settles the seconds relayed until then, rounded up once,
// flagged partial (docs/specs/GATEWAY.md, Accounting → Partial).
func TestSpeechClientGoneMidClip(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	pace := make(chan struct{})
	g.backend.SetReply(fakebackend.Reply{Pace: pace})
	url := serveGateway(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := speechRequest(t, ctx, url, `{"model":"tts","input":"hello","stream_format":"audio","response_format":"pcm","sample_rate":8000}`)
	// Four pieces of 0.625 s (10 000 bytes at 8 kHz): read two, then leave.
	piece := make([]byte, 10000)
	if _, err := io.ReadFull(resp.Body, piece); err != nil {
		t.Fatal(err)
	}
	pace <- struct{}{}
	if _, err := io.ReadFull(resp.Body, piece); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	rec := settledRecord(t, g)
	if rec.Units[config.UnitAudioSecondsOut] != 2 || !rec.Partial || !rec.Estimated {
		t.Errorf("record %+v: want 2 s (1.25 s relayed, rounded up), partial, estimated", rec)
	}
}

// speechRequest sends a speech request of the eval group's key to the gateway at url.
func speechRequest(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/v1/audio/speech", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The whole clip takes the response timeout, not retried; the raw-audio stream the
// stream timers: its body reads are its progress, so a backend silent past the stall
// timeout after its first audio has stalled — the relay cut, the seconds relayed
// settled partial; the SSE stream the stream timers as every event stream does (the
// next test) (docs/specs/GATEWAY.md, Routing and reliability → Timeouts).
func TestSpeechTimers(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	w := do(t, g.h, speechCall(`{"model":"tts-slow","input":"hi"}`, "whole-slow"))
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1: a response timeout is not retried", n)
	}
	if line := logLine(t, g, "whole-slow"); !strings.Contains(line, "no response within 150ms") ||
		!strings.Contains(line, `"gen_ai.request.stream":false`) {
		t.Errorf("log line: %s", line)
	}

	g = newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	url := serveGateway(t, g)
	start := time.Now()
	resp := speechRequest(t, context.Background(), url,
		`{"model":"tts-slow","input":"hi","stream_format":"audio","response_format":"pcm","sample_rate":8000}`)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || len(data) != 10000 || time.Since(start) > waitTimeout {
		t.Fatalf("read %d bytes, ended %v after %s: want the first piece, then the connection cut", len(data), err, time.Since(start))
	}
	rec := settledRecord(t, g)
	if rec.Units[config.UnitAudioSecondsOut] != 1 || !rec.Partial {
		t.Errorf("record %+v: want 1 s (0.625 s relayed), partial", rec)
	}
	if logs := g.logText(); !strings.Contains(logs, `"kaiak.relay_end":"upstream_stalled"`) {
		t.Errorf("log misses the stall:\n%s", logs)
	}
}

// A WAV raw-audio stream opens with its header together with its first audio, as
// vLLM-Omni yields them, so a backend that hangs before its first audio sent nothing:
// a 504 upstream_timeout at the first-event timeout, the prompt billed as sent, no
// audio (docs/specs/GATEWAY.md, Routing and reliability → Timeouts; Providers → media
// answers).
func TestSpeechRawAudioHangBeforeTheFirstAudio(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"first_event_timeout_ms": 5000`, `"first_event_timeout_ms": 150`)
	})
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.Hang}})
	body := `{"model":"tts-slow","input":"hi","stream_format":"audio"}`
	w := do(t, g.h, speechCall(body, "raw-first"))
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if line := logLine(t, g, "raw-first"); !strings.Contains(line, "no first event within 150ms") {
		t.Errorf("log line: %s", line)
	}
	in := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
	expectUnits(t, onlyRecord(t, g), speechUnits(in, 0, 0), true, true)
}

// The SSE stream takes the stream timers: a backend that sends no first event within
// the first-event timeout is a 504 upstream_timeout, the prompt billed as sent; one
// silent past the stall timeout after its first events has stalled — the relay cut,
// the seconds its deltas relayed settled, the tokens estimated, partial
// (docs/specs/GATEWAY.md, Routing and reliability → Timeouts).
func TestSpeechEventStreamTimers(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"first_event_timeout_ms": 5000`, `"first_event_timeout_ms": 150`)
	})
	g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
	body := `{"model":"tts-slow","input":"hi","stream":true}`
	w := do(t, g.h, speechCall(body, "sse-first"))
	expectError(t, w, http.StatusGatewayTimeout, "upstream_timeout")
	if line := logLine(t, g, "sse-first"); !strings.Contains(line, "no first event within 150ms") ||
		!strings.Contains(line, `"gen_ai.request.stream":true`) {
		t.Errorf("log line: %s", line)
	}
	in := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
	expectUnits(t, onlyRecord(t, g), speechUnits(in, 0, 0), true, true)

	g = newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.Hang}})
	url := serveGateway(t, g)
	start := time.Now()
	resp := speechRequest(t, context.Background(), url, body)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || time.Since(start) > waitTimeout {
		t.Fatalf("ended %v after %s: want the connection cut", err, time.Since(start))
	}
	if names, audio, _ := speechEvents(t, string(data)); len(names) != 2 || len(audio) != 44+30000 {
		t.Errorf("relayed %v, %d bytes of audio: want the header and the first piece", names, len(audio))
	}
	expectUnits(t, settledRecord(t, g), speechUnits(in, 0, 1), true, true)
	waitLog(t, g, `"kaiak.relay_end":"upstream_stalled"`)
}

// An SSE stream that ends before speech.audio.done — the connection cut, or ended
// cleanly without it — is a backend failure: the relay cut (upstream_failed,
// upstream_incomplete), the audio its deltas relayed settled, rounded up, the tokens
// estimated, partial. A done event without usage completes the stream, its tokens
// estimated (docs/specs/GATEWAY.md, Providers: complete responses; Accounting →
// Media usage, Partial).
func TestSpeechEventStreamEnds(t *testing.T) {
	body := `{"model":"tts","input":"hello","stream":true}`
	in := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
	for _, c := range []struct {
		name     string
		kind     fakebackend.StreamFaultKind
		relayEnd string
	}{
		{"cut", fakebackend.Cut, "upstream_failed"},
		{"ended without done", fakebackend.End, "upstream_incomplete"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g)
			g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: c.kind}})
			resp := speechRequest(t, context.Background(), serveGateway(t, g), body)
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				t.Errorf("the stream ended cleanly; want the connection cut")
			}
			// The header, then two pieces of 0.625 s: 1.25 s relayed.
			if names, audio, _ := speechEvents(t, string(data)); len(names) != 3 || len(audio) != 44+60000 {
				t.Errorf("relayed %v, %d bytes of audio: want the header and two pieces", names, len(audio))
			}
			expectUnits(t, settledRecord(t, g), speechUnits(in, 0, 2), true, true)
			waitLog(t, g, `"kaiak.relay_end":"`+c.relayEnd+`"`)
			expectMetricLines(t, scrape(g), `kaiak_upstream_attempts_total{kaiak_backend_id="omni",`+
				`kaiak_deployment_model="Qwen/Qwen3-TTS",kaiak_attempt_outcome="broke_off"} 1`)
		})
	}

	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{OmitUsage: true})
	w := do(t, g.h, speechCall(body, "no-usage"))
	if names, _, last := speechEvents(t, w.Body.String()); w.Code != http.StatusOK || names[len(names)-1] != "speech.audio.done" ||
		strings.Contains(last, "usage") {
		t.Fatalf("%d %v %s: want the stream whole, done without usage", w.Code, names, last)
	}
	expectUnits(t, onlyRecord(t, g), speechUnits(in, 0, 3), true, false)
}

// speech.audio.error after the first event reaches the client — its type and code,
// partial_audio and action kept, its message the gateway's — then the connection is
// cut: the stream ended incomplete, a backend failure (broke_off), the audio relayed
// settled with the tokens estimated, partial. As the first event it is the backend
// failing before anything reached the client: retried on another deployment, the
// failed attempt billing nothing, and answered 502 upstream_error with its code on the
// log line when no deployment is left (docs/specs/GATEWAY.md, Providers: error
// events).
func TestSpeechEventStreamErrorEvents(t *testing.T) {
	body := `{"model":"tts","input":"hello","stream":true}`
	in := accounting.EstimateInput(provider.AudioSpeech, []byte(body)).Total
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 2, Kind: fakebackend.ErrorEvent}})
	resp := speechRequest(t, context.Background(), serveGateway(t, g), body)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Errorf("the stream ended cleanly; want the connection cut")
	}
	names, audio, last := speechEvents(t, string(data))
	if len(names) != 4 || names[3] != "speech.audio.error" || len(audio) != 44+60000 {
		t.Errorf("relayed %v, %d bytes of audio: want the header, two pieces, then the error", names, len(audio))
	}
	for _, want := range []string{`"type":"speech.audio.error"`, `"type":"server_error"`, `"code":500`, `"partial_audio":true`,
		`"action":"discard"`, `"message":"The model backend ended the response with an error."`} {
		if !strings.Contains(last, want) {
			t.Errorf("relayed error %s lacks %s", last, want)
		}
	}
	if strings.Contains(string(data), "CUDA") || strings.Contains(string(data), "Qwen") {
		t.Errorf("the backend's message reached the client: %s", last)
	}
	expectUnits(t, settledRecord(t, g), speechUnits(in, 0, 2), true, true)
	waitLog(t, g, `"kaiak.relay_end":"upstream_incomplete"`)
	expectMetricLines(t, scrape(g), `kaiak_upstream_attempts_total{kaiak_backend_id="omni",`+
		`kaiak_deployment_model="Qwen/Qwen3-TTS",kaiak_attempt_outcome="broke_off"} 1`)

	// The first event: retried on the second deployment, which answers.
	other := fakebackend.New()
	t.Cleanup(other.Close)
	g = newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "omni-b": { "type": "vllm-omni", "base_url": "`+other.URL()+`/v1" },`)
		return replaceOnce(t, doc, `[{ "backend": "omni", "model": "Qwen/Qwen3-TTS" }]`,
			`[{ "backend": "omni", "model": "Qwen/Qwen3-TTS" }, { "backend": "omni-b", "model": "Qwen/Qwen3-TTS" }]`)
	})
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.ErrorEvent}})
	w := do(t, g.h, speechCall(body, "first"))
	if w.Code != http.StatusOK || len(g.backend.Requests())+len(other.Requests()) != 2 {
		t.Fatalf("%d, %d and %d backend requests: want 200, after one attempt on each deployment", w.Code,
			len(g.backend.Requests()), len(other.Requests()))
	}
	records := recordsOf(g, "first")
	if len(records) != 1 {
		t.Fatalf("%d records, want the answer's alone: the error event billed nothing", len(records))
	}
	expectUnits(t, records[0], speechUnits(7, 4, 3), false, false)
	expectFields(t, logFields(t, g, "first"), map[string]any{
		"kaiak.tried": "omni/Qwen/Qwen3-TTS:upstream_error,omni-b/Qwen/Qwen3-TTS:200"})
	expectMetricLines(t, scrape(g),
		`kaiak_retries_total{gen_ai_request_model="tts",kaiak_backend_id="omni",kaiak_attempt_outcome="server_error"} 1`)

	// No deployment left: 502 upstream_error, the event's code on the log line.
	g = newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 0, Kind: fakebackend.ErrorEvent}})
	w = do(t, g.h, speechCall(body, "first-alone"))
	expectError(t, w, http.StatusBadGateway, "upstream_error")
	expectFields(t, logFields(t, g, "first-alone"), map[string]any{"error.type": "upstream_error",
		"kaiak.upstream.error.code": "500", "gen_ai.request.stream": true})
	expectUnits(t, onlyRecord(t, g), speechUnits(0, 0, 0), false, true)
	if strings.Contains(g.logText(), "CUDA") {
		t.Errorf("the backend's message reached the log:\n%s", g.logText())
	}
}

// The limits on audio generated count a speech request's audio_seconds_out once it
// settles, per minute and per hour, reserving nothing before it runs: a window
// reached refuses the next speech request, 429 audio_seconds — not a chat request
// (docs/specs/GATEWAY.md, Limits → Check and reserve, Settle).
func TestSpeechAudioLimits(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitAudioSecondsOutPerMinute, config.LimitAudioSecondsOutPerHour} {
		t.Run(string(typ), func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g, func(doc string) string {
				return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "`+string(typ)+`", "value": 5 }] }`)
			})
			body := `{"model":"tts","input":"hi"}`
			for i := range 2 {
				if w := do(t, g.h, speechCall(body, "ok")); w.Code != http.StatusOK {
					t.Fatalf("request %d: %d %s", i+1, w.Code, w.Body.String())
				}
			}
			if used := counterUsed(t, g, "research", typ); used != 6 {
				t.Errorf("window holds %d s, want 6: two clips of 2.5 s, each rounded up", used)
			}
			w := do(t, g.h, speechCall(body, "refused"))
			expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
			if typ, _, _ := openAIError(t, w); typ != "audio_seconds" || w.Header().Get("Retry-After") == "" {
				t.Errorf("type %q, Retry-After %q: want audio_seconds and a wait", typ, w.Header().Get("Retry-After"))
			}
			if !strings.Contains(errorMessage(t, w), "group limit of 5 seconds of audio generated") {
				t.Errorf("message %q", errorMessage(t, w))
			}
			if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
				t.Errorf("chat refused by the audio limit: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// Speech to a model whose server has no speech stage is the backend's 400, relayed
// as the caller's: not retried, nothing settled but zeros (docs/specs/GATEWAY.md,
// Providers → An endpoint missing from a server).
func TestSpeechToAModelThatDoesNotSpeak(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	w := do(t, g.h, speechCall(`{"model":"painter","input":"hi"}`, "painter"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "TTS model did not produce audio output.") {
		t.Fatalf("%d %s, want the backend's 400 relayed", w.Code, w.Body.String())
	}
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
	expectUnits(t, onlyRecord(t, g), speechUnits(0, 0, 0), false, false)
}

// Nothing of the text to speak, the reference audio, the audio answered — as bytes or
// as base64 in events — the word timing or a backend's error text reaches the log
// (docs/specs/GATEWAY.md, Observability: logs).
func TestSpeechLogsHoldNoContent(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Header: map[string]string{"X-Word-Timestamps": `[{"word":"zanzibar"}]`}})
	body := `{"model":"tts","input":"zanzibar quokka","instructions":"whisper marmalade",` +
		`"ref_audio":"data:audio/wav;base64,UklGRnF1b2trYQ==","ref_text":"pomegranate"}`
	for _, c := range []call{speechCall(body, "secret-1"),
		speechCall(strings.Replace(body, `"model":"tts"`, `"stream_format":"audio","model":"tts"`, 1), "secret-2"),
		speechCall(strings.Replace(body, `"ref_text"`, `"ref_audio_2":"https://zanzibar.example/a.wav","ref_text"`, 1), "secret-3"),
		speechCall(strings.Replace(body, `"model":"tts"`, `"stream":true,"model":"tts"`, 1), "secret-4")} {
		do(t, g.h, c)
	}
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{At: 1, Kind: fakebackend.ErrorEvent}})
	resp := speechRequest(t, context.Background(), serveGateway(t, g), strings.Replace(body, `"model":"tts"`, `"stream":true,"model":"tts"`, 1))
	_, _ = io.Copy(io.Discard, resp.Body) // the connection is cut after the error event
	resp.Body.Close()
	waitLog(t, g, `"kaiak.relay_end":"upstream_incomplete"`)
	logs := g.logText()
	// UklGR begins a WAV header in base64, as an SSE stream's first delta carries it.
	for _, secret := range []string{"zanzibar", "quokka", "marmalade", "UklGRnF1b2trYQ", "pomegranate", "RIFF", "UklGR", "CUDA"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}
