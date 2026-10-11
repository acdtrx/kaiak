package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// batchCall is a speech batch request of the eval group's key carrying body, request
// ID id.
func batchCall(body, id string) call {
	return call{method: "POST", path: "/v1/audio/speech/batch", key: workloadKey, body: body,
		header: map[string]string{"X-Request-Id": id}}
}

// batchResults reads a speech batch answer as the client got it: each result's status
// and its audio, decoded.
func batchResults(t *testing.T, body []byte) (statuses []string, audio [][]byte) {
	t.Helper()
	var answer struct {
		Results []struct {
			Index     int    `json:"index"`
			Status    string `json:"status"`
			AudioData string `json:"audio_data"`
			MediaType string `json:"media_type"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("batch answer %.200s: %v", body, err)
	}
	for i, r := range answer.Results {
		if r.Index != i {
			t.Errorf("result %d has index %d", i, r.Index)
		}
		decoded, err := base64.StdEncoding.DecodeString(r.AudioData)
		if err != nil {
			t.Fatalf("result %d's audio: %v", i, err)
		}
		statuses, audio = append(statuses, r.Status), append(audio, decoded)
	}
	return statuses, audio
}

// A speech batch passes the whole pipeline to a vllm-omni deployment: sent with the
// model edit alone — the model's default output limit is reserved, never sent —
// everything else as the client wrote it; answered
// with the backend's JSON as sent — each item's audio in its format — never a stream;
// settled from the items' usage summed, the audio of every item measured — WAV and
// PCM at the item's rate — and summed before one rounding (3 × 1.25 s → 4 s), priced on
// its tokens; operation generate_content and output type speech on the request line
// and the usage metrics, the batch never a stream there and never timed to a first
// token (docs/specs/GATEWAY.md, Client API → owned fields: speech batch; Providers;
// Accounting → Media usage; Observability).
func TestSpeechBatchThroughThePipeline(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{AudioMillis: 1250})
	body := `{"model":"tts","voice":"vivian","items":[{"input":"one"},{"input":"two","max_new_tokens":1000},` +
		`{"input":"three","response_format":"pcm","sample_rate":8000,"max_new_tokens":200}],"stream":true}`
	w := do(t, g.h, batchCall(body, "batch-1"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s: %.300s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	statuses, audio := batchResults(t, w.Body.Bytes())
	if strings.Join(statuses, ",") != "success,success,success" || len(audio[0]) != 44+60000 || len(audio[1]) != 44+60000 ||
		len(audio[2]) != 20000 || !strings.HasPrefix(string(audio[0]), "RIFF") {
		t.Errorf("results %v with %d, %d and %d bytes of audio: want three, two WAV files of 1.25 s and 1.25 s of PCM at 8 kHz",
			statuses, len(audio[0]), len(audio[1]), len(audio[2]))
	}

	reqs := g.backend.Requests()
	want := `{"model":"Qwen/Qwen3-TTS","voice":"vivian","items":[{"input":"one"},{"input":"two","max_new_tokens":1000},` +
		`{"input":"three","response_format":"pcm","sample_rate":8000,"max_new_tokens":200}],"stream":true}`
	if len(reqs) != 1 || reqs[0].Path != "/v1/audio/speech/batch" || string(reqs[0].Body) != want {
		t.Fatalf("backend requests %+v, want one to /v1/audio/speech/batch with\n %s", reqs, want)
	}

	rec := onlyRecord(t, g)
	expectUnits(t, rec, speechUnits(21, 12, 4), false, false)
	if rec.Operation != "generate_content" || rec.OutputType != "speech" || rec.CostNanoUSD != 21*1000+12*2000 {
		t.Errorf("record operation %q, output type %q, cost %d; want generate_content, speech, %d", rec.Operation,
			rec.OutputType, rec.CostNanoUSD, 21*1000+12*2000)
	}
	line := logLine(t, g, "batch-1")
	for _, want := range []string{`"gen_ai.operation.name":"generate_content"`, `"gen_ai.output.type":"speech"`,
		`"gen_ai.request.stream":false`, `"kaiak.backend.type":"vllm-omni"`, `"kaiak.usage.audio.output.duration":4`,
		`"gen_ai.usage.input_tokens":21`, `"gen_ai.usage.output_tokens":12`} {
		if !strings.Contains(line, want) {
			t.Errorf("log line lacks %s: %s", want, line)
		}
	}
	usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="tts",` +
		`gen_ai_operation_name="generate_content",gen_ai_output_type="speech",kaiak_usage_status="complete"}`
	text := g.metricsText()
	expectMetricLines(t, text,
		`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",http_route="/v1/audio/speech/batch",`+
			`http_response_status_code="200",gen_ai_request_model="tts"} 1`,
		`kaiak_usage_records_total`+usage+` 1`,
		`kaiak_usage_audio_output_duration_seconds_total`+usage+` 4`)
	if strings.Contains(text, `kaiak_time_to_first_token_seconds_count{gen_ai_request_model="tts"`) {
		t.Errorf("a batch was timed to a first token")
	}
}

// What the gateway refuses on /v1/audio/speech/batch it refuses before routing, in
// OpenAI's error shape, naming the member at fault — an item's by its position: more
// items than max_speech_batch_items; audio it cannot measure, from the item's own
// format or the batch's it inherits; PCM without the rate the item gets; a reference
// audio the backend would fetch or read, at either level; owned fields of the wrong
// type or named twice; an output limit out of range or above the model's ceiling, the
// batch's or an item's — the batch's even when every item overrides it; a model on no
// vllm-omni deployment (docs/specs/GATEWAY.md, Client API → owned fields: speech
// batch; errors).
func TestSpeechBatchRefusals(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	const inline = `"data:audio/wav;base64,UklGRg=="`
	items := func(n int) string {
		return `[` + strings.TrimSuffix(strings.Repeat(`{"input":"x"},`, n), ",") + `]`
	}
	for _, c := range []struct {
		name, body, code, param string
	}{
		{"more items than the cap", `{"model":"tts","items":` + items(33) + `}`, "invalid_value", "items"},
		{"an item's format", `{"model":"tts","items":[{"input":"a"},{"input":"b","response_format":"mp3"}]}`,
			"response_format_unsupported", "items[1].response_format"},
		{"the batch's format, inherited", `{"model":"tts","response_format":"opus","items":[{"input":"a","response_format":"wav"},` +
			`{"input":"b"}]}`, "response_format_unsupported", "response_format"},
		{"an item's PCM without a rate", `{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c","response_format":"pcm"}]}`,
			"missing_required_parameter", "items[2].sample_rate"},
		{"the batch's PCM without a rate", `{"model":"tts","response_format":"pcm","items":[{"input":"a","sample_rate":8000},` +
			`{"input":"b"}]}`, "missing_required_parameter", "sample_rate"},
		{"an item's ref_audio URL", `{"model":"tts","items":[{"input":"a","ref_audio":"https://example.com/v.wav"}]}`,
			"external_reference_unsupported", "items[0].ref_audio"},
		{"the batch's ref_audio path", `{"model":"tts","ref_audio":"/srv/voices/a.wav","items":[{"input":"a","ref_audio":` + inline + `}]}`,
			"external_reference_unsupported", "ref_audio"},
		{"an item's ref_audio a list", `{"model":"tts","items":[{"input":"a","ref_audio":[` + inline + `]}]}`,
			"invalid_type", "items[0].ref_audio"},
		{"the batch's ref_audio a list", `{"model":"tts","ref_audio":[` + inline + `],"items":[{"input":"a"}]}`, "invalid_type", "ref_audio"},
		{"an item's format not a string", `{"model":"tts","items":[{"input":"a","response_format":1}]}`, "invalid_type",
			"items[0].response_format"},
		{"the batch's format not a string", `{"model":"tts","response_format":["wav"],"items":[{"input":"a"}]}`, "invalid_type",
			"response_format"},
		{"an item's rate not an integer", `{"model":"tts","items":[{"input":"a"},{"input":"b","sample_rate":"8000"}]}`,
			"invalid_type", "items[1].sample_rate"},
		{"an item's output limit not an integer", `{"model":"tts","items":[{"input":"a","max_new_tokens":"many"}]}`,
			"invalid_type", "items[0].max_new_tokens"},
		{"an item's output limit negative", `{"model":"tts","items":[{"input":"a","max_new_tokens":-1}]}`,
			"invalid_value", "items[0].max_new_tokens"},
		{"an item's output limit above the context", `{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c"},` +
			`{"input":"d","max_new_tokens":8193}]}`, "invalid_value", "items[3].max_new_tokens"},
		{"an item's output limit above the ceiling", `{"model":"tts","items":[{"input":"a","max_new_tokens":1000},` +
			`{"input":"b","max_new_tokens":1001}]}`, "invalid_value", "items[1].max_new_tokens"},
		{"the batch's output limit above the ceiling", `{"model":"tts","max_new_tokens":1001,"items":[{"input":"a","max_new_tokens":5}]}`,
			"invalid_value", "max_new_tokens"},
		{"the batch's output limit negative", `{"model":"tts","max_new_tokens":-1,"items":[{"input":"a"}]}`,
			"invalid_value", "max_new_tokens"},
		{"items not a list", `{"model":"tts","items":{"input":"a"}}`, "invalid_type", "items"},
		{"an item not an object", `{"model":"tts","items":[{"input":"a"},"b"]}`, "invalid_type", "items[1]"},
		{"an item naming a member twice", `{"model":"tts","items":[{"input":"a","input":"b"}]}`, "duplicate_member", "items[0].input"},
		{"items named twice", `{"model":"tts","items":[{"input":"a"}],"items":[]}`, "duplicate_member", "items"},
		{"model missing", `{"items":[{"input":"a"}]}`, "missing_required_parameter", "model"},
		{"not on vllm-omni", `{"model":"open","items":[{"input":"a"}]}`, "endpoint_not_served", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, batchCall(c.body, "refused"))
			expectError(t, w, http.StatusBadRequest, c.code)
			if _, _, param := openAIError(t, w); (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q: %s", param, c.param, w.Body.String())
			}
			if msg := errorMessage(t, w); strings.Contains(msg, "example.com") || strings.Contains(msg, "/srv/") {
				t.Errorf("message %q echoes the value", msg)
			}
			if c.code == "endpoint_not_served" && !strings.Contains(errorMessage(t, w), "/v1/audio/speech/batch") {
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
		return replaceOnce(t, doc, `"global": { `, `"global": { "max_speech_batch_items": 2, `)
	})
	w := do(t, g.h, batchCall(`{"model":"tts","items":`+items(3)+`}`, "over"))
	expectError(t, w, http.StatusBadRequest, "invalid_value")
	if msg := errorMessage(t, w); !strings.Contains(msg, "'items' holds 3 items; the gateway's maximum is 2") {
		t.Errorf("message %q", msg)
	}
	if w := do(t, g.h, batchCall(`{"model":"tts","items":`+items(2)+`}`, "at")); w.Code != http.StatusOK {
		t.Errorf("two items under a cap of 2: %d %s", w.Code, w.Body.String())
	}
}

// The items cap counts a batch's items before it decodes any: refusing 200 000 empty
// items — 600 001 bytes, within the default body size cap — allocates what refusing
// 1 000 does, none of the items held (decoding them first costs an allocation per
// item; the margin is other goroutines' allocations meanwhile, which the count takes
// in); a list within the cap is decoded into its items (docs/specs/GATEWAY.md, Limits →
// Output multiplicity: batch caps).
func TestSpeechBatchCapCountsItemsBeforeDecodingThem(t *testing.T) {
	allocs := func(n int) float64 {
		raw := json.RawMessage(`[` + strings.Repeat(`{},`, n-1) + `{}]`)
		if _, apiErr := speechBatchItems(raw, 32); apiErr == nil || apiErr.code != "invalid_value" ||
			!strings.Contains(apiErr.message, "holds "+strconv.Itoa(n)+" items") {
			t.Fatalf("%d items over a cap of 32: %+v, want invalid_value naming %d", n, apiErr, n)
		}
		return testing.AllocsPerRun(5, func() { _, _ = speechBatchItems(raw, 32) })
	}
	const margin = 1_000
	if few, many := allocs(1_000), allocs(200_000); many > few+margin {
		t.Errorf("refusing 1 000 items allocates %v times, 200 000 items %v: the items are decoded before the cap", few, many)
	}

	items, apiErr := speechBatchItems(json.RawMessage(` [ {"input":"a"} , "b", [1,{"c":"]"}] ] `), 32)
	if apiErr != nil || len(items) != 3 || string(items[0]) != `{"input":"a"}` || string(items[2]) != `[1,{"c":"]"}]` {
		t.Errorf("three items within the cap: %q %+v", items, apiErr)
	}
}

// What the gateway does not own passes untouched, and so do the values it reads
// when they keep to the rules: the batch's defaults where items override them all, a
// rate an item's PCM inherits from the batch, inline reference audio at either level,
// null values as absent, stream and stream_format — a batch has none, so it is timed
// and logged as no stream — and extra_params at either level, which vLLM-Omni's batch
// drops unread.
func TestSpeechBatchPassesWhatItDoesNotOwn(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	const inline = `"data:audio/wav;base64,UklGRg=="`
	for _, body := range []string{
		`{"model":"tts-free","response_format":"mp3","items":[{"input":"a","response_format":"wav"}]}`,
		`{"model":"tts-free","response_format":"pcm","sample_rate":8000,"items":[{"input":"a"},{"input":"b","response_format":"wav"}]}`,
		`{"model":"tts-free","sample_rate":8000,"items":[{"input":"a","response_format":"pcm"}]}`,
		`{"model":"tts-free","ref_audio":` + inline + `,"items":[{"input":"a","ref_audio":"DATA:audio/wav;base64,UklGRg=="},{"input":"b"}]}`,
		`{"model":"tts-free","ref_audio":null,"response_format":null,"sample_rate":null,"max_new_tokens":null,` +
			`"items":[{"input":"a","ref_audio":null,"response_format":null,"sample_rate":null,"max_new_tokens":null}]}`,
		`{"model":"tts-free","stream":true,"stream_format":"sse","voice":"vivian","task_type":"Base","extra":{"model":"keep"},` +
			`"items":[{"input":"a","voice":"x","instructions":"calm","speed":1.5,"x_vector_only_mode":true,"stream":true}]}`,
		`{"model":"tts-free","extra_params":{"emo_alpha":0.5},"items":[{"input":"a","extra_params":{"emo_alpha":0.7}},{"input":"b"}]}`,
	} {
		w := do(t, g.h, batchCall(body, "pass"))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
			continue
		}
		reqs := g.backend.Requests()
		if sent, want := string(reqs[len(reqs)-1].Body), strings.Replace(body, `"tts-free"`, `"Qwen/Qwen3-TTS-free"`, 1); sent != want {
			t.Errorf("sent\n %s\nwant\n %s", sent, want)
		}
	}
	if line := logLine(t, g, "pass"); !strings.Contains(line, `"gen_ai.request.stream":false`) {
		t.Errorf("a batch naming stream logged as a stream: %s", line)
	}
}

// A batch's output limits (docs/specs/GATEWAY.md, Limits → Output-limit keys, Output
// multiplicity): nothing is ever written — a batch that sets none is sent without one,
// vLLM-Omni budgets each item itself, and an item without its own reserves the model's
// default; the batch's own value and each item's are kept, each checked on its own
// (above the ceiling refused: TestSpeechBatchRefusals); a model with no output limit
// reserves what the client set. The reservation counts each item's effective limit
// once: a token limit too small for the items refuses the batch as too large.
func TestSpeechBatchOutputLimits(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		reserved         int64
	}{
		{"the default reserved, unsent", `{"model":"tts","items":[{"input":"a"},{"input":"b","max_new_tokens":50},` +
			`{"input":"c","max_new_tokens":1000}]}`,
			`{"model":"Qwen/Qwen3-TTS","items":[{"input":"a"},{"input":"b","max_new_tokens":50},` +
				`{"input":"c","max_new_tokens":1000}]}`, 300 + 50 + 1000},
		{"the batch's own, at the ceiling", `{"model":"tts","max_new_tokens":1000,"items":[{"input":"a"},{"input":"b"}]}`,
			`{"model":"Qwen/Qwen3-TTS","max_new_tokens":1000,"items":[{"input":"a"},{"input":"b"}]}`, 2 * 1000},
		{"the batch's own, kept", `{"model":"tts","max_new_tokens":200,"items":[{"input":"a"},{"input":"b","max_new_tokens":20}]}`,
			`{"model":"Qwen/Qwen3-TTS","max_new_tokens":200,"items":[{"input":"a"},{"input":"b","max_new_tokens":20}]}`, 200 + 20},
		{"no output limit declared", `{"model":"tts-free","items":[{"input":"a"},{"input":"b","max_new_tokens":7000}]}`,
			`{"model":"Qwen/Qwen3-TTS-free","items":[{"input":"a"},{"input":"b","max_new_tokens":7000}]}`, 7000},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g, func(doc string) string {
				return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
			})
			w := do(t, g.h, batchCall(c.body, "limit"))
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			reqs := g.backend.Requests()
			if sent := string(reqs[len(reqs)-1].Body); sent != c.want {
				t.Errorf("sent\n %s\nwant\n %s", sent, c.want)
			}
			estimate := accounting.EstimateInput(provider.AudioSpeechBatch, []byte(c.body)).Total
			if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.FormatInt(100000-estimate-c.reserved, 10) {
				t.Errorf("remaining tokens at admission %s, want the reservation of %d + %d taken", got, estimate, c.reserved)
			}
		})
	}

	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "tokens_per_minute", "value": 1000 }] }`)
	})
	if w := do(t, g.h, batchCall(`{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c"}]}`, "fits")); w.Code != http.StatusOK {
		t.Errorf("three items of 300: %d %s", w.Code, w.Body.String())
	}
	w := do(t, g.h, batchCall(`{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c"},{"input":"d"}]}`, "too-large"))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(errorMessage(t, w), "Request too large") {
		t.Errorf("four items of 300: %d %s, want 429, too large for 1000 tokens", w.Code, w.Body.String())
	}
}

// An errored item adds nothing — its usage or audio — while the others count; a
// successful item without usage leaves the record's tokens estimated, its audio still
// measured; a batch every item of which errored settles zeros (docs/specs/GATEWAY.md,
// Accounting → Media usage).
func TestSpeechBatchErroredItemsAndMissingUsage(t *testing.T) {
	body := `{"model":"tts","items":[{"input":"one"},{"input":"two"},{"input":"three"}]}`
	in := accounting.EstimateInput(provider.AudioSpeechBatch, []byte(body)).Total
	for _, c := range []struct {
		name      string
		reply     fakebackend.Reply
		statuses  string
		want      accounting.Units
		estimated bool
	}{
		{"an errored item", fakebackend.Reply{FailItems: []int{1}}, "success,error,success", speechUnits(14, 8, 5), false},
		{"every item errored", fakebackend.Reply{FailItems: []int{0, 1, 2}}, "error,error,error", speechUnits(0, 0, 0), false},
		{"no usage", fakebackend.Reply{OmitUsage: true}, "success,success,success", speechUnits(in, 0, 8), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withSpeechModels(t, g)
			g.backend.SetReply(c.reply)
			w := do(t, g.h, batchCall(body, "items"))
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if statuses, _ := batchResults(t, w.Body.Bytes()); strings.Join(statuses, ",") != c.statuses {
				t.Errorf("results %v, want %s", statuses, c.statuses)
			}
			expectUnits(t, onlyRecord(t, g), c.want, c.estimated, false)
		})
	}
}

// A batch answer of several MiB — eight items of 10 s, about 5 MiB — is relayed whole
// and counted as it passes: 80 s, the tokens summed (docs/specs/GATEWAY.md, Accounting
// → Media usage).
func TestSpeechBatchOfSeveralMiB(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{AudioMillis: 10000})
	body := `{"model":"tts","items":[` + strings.TrimSuffix(strings.Repeat(`{"input":"x"},`, 8), ",") + `]}`
	w := do(t, g.h, batchCall(body, "large"))
	if w.Code != http.StatusOK || w.Body.Len() < 4<<20 {
		t.Fatalf("%d, %d bytes: want 200 and over 4 MiB", w.Code, w.Body.Len())
	}
	if _, audio := batchResults(t, w.Body.Bytes()); len(audio) != 8 || len(audio[7]) != 44+480000 {
		t.Fatalf("%d results, the last %d bytes: want eight WAV files of 10 s", len(audio), len(audio[len(audio)-1]))
	}
	expectUnits(t, onlyRecord(t, g), speechUnits(56, 32, 80), false, false)
}

// An item's own sample_rate wins over the batch's, as vLLM-Omni merges them: its PCM
// comes at the item's rate and is measured at it — 2.5 s at 8 kHz, 3 s once rounded
// up; read at the batch's 24 kHz it would be 1 (docs/specs/GATEWAY.md, Client API →
// owned fields: speech batch).
func TestSpeechBatchItemRateWinsOverTheBatchs(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{AudioMillis: 2500})
	body := `{"model":"tts","sample_rate":24000,"items":[{"input":"a","response_format":"pcm","sample_rate":8000}]}`
	if w := do(t, g.h, batchCall(body, "rates")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if rec := onlyRecord(t, g); rec.Units[config.UnitAudioSecondsOut] != 3 || rec.Estimated {
		t.Errorf("record %+v: want 3 s, measured at the item's 8 kHz", rec)
	}
}

// The limits on audio generated count a batch's seconds once it settles: a window
// reached refuses the next batch, 429 audio_seconds (docs/specs/GATEWAY.md, Limits →
// Check and reserve, Settle).
func TestSpeechBatchAudioLimits(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": {}`, `"research": { "limits": [{ "type": "audio_seconds_out_per_minute", "value": 5 }] }`)
	})
	body := `{"model":"tts","items":[{"input":"a"},{"input":"b"},{"input":"c"}]}`
	if w := do(t, g.h, batchCall(body, "ok")); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if used := counterUsed(t, g, "research", config.LimitAudioSecondsOutPerMinute); used != 8 {
		t.Errorf("window holds %d s, want 8: three clips of 2.5 s, summed and rounded up once", used)
	}
	w := do(t, g.h, batchCall(body, "refused"))
	expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
	if typ, _, _ := openAIError(t, w); typ != "audio_seconds" {
		t.Errorf("type %q, want audio_seconds", typ)
	}
}

// A batch to a model whose server cannot speak — its list should leave the endpoint
// out — is the backend's 400, relayed as the caller's: not retried, nothing settled but
// zeros (docs/specs/GATEWAY.md, Model metadata → Endpoints per model; Providers → A
// path the server does not have).
func TestSpeechBatchToAModelThatDoesNotSpeak(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	w := do(t, g.h, batchCall(`{"model":"painter","items":[{"input":"hi"}]}`, "painter"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Batch speech is not supported in diffusion mode") {
		t.Fatalf("%d %s, want the backend's 400 relayed", w.Code, w.Body.String())
	}
	if n := len(g.backend.Requests()); n != 1 {
		t.Errorf("backend got %d requests, want 1", n)
	}
	expectUnits(t, onlyRecord(t, g), speechUnits(0, 0, 0), false, false)
}

// Nothing of the texts to speak, the reference audio, the audio answered or an item's
// error text reaches the log (docs/specs/GATEWAY.md, Observability: logs).
func TestSpeechBatchLogsHoldNoContent(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{FailItems: []int{1}})
	body := `{"model":"tts","instructions":"whisper marmalade","ref_audio":"data:audio/wav;base64,UklGRnF1b2trYQ==",` +
		`"items":[{"input":"zanzibar quokka","ref_text":"pomegranate"},{"input":"second"}]}`
	if w := do(t, g.h, batchCall(body, "secret")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"error"`) {
		t.Fatalf("%d %.300s: want the answer, an item errored", w.Code, w.Body.String())
	}
	do(t, g.h, batchCall(strings.Replace(body, `"ref_text"`, `"ref_audio":"https://zanzibar.example/a.wav","ref_text"`, 1), "secret-2"))
	logs := g.logText()
	for _, secret := range []string{"zanzibar", "quokka", "marmalade", "UklGRnF1b2trYQ", "pomegranate", "UklGR", "CUDA"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}

// An errored item's text is the gateway's, never the backend's — which can name hosts,
// devices or the backend-side model — and reaches neither the client nor the log; the
// result keeps its index and status, and the others pass as sent (docs/specs/GATEWAY.md,
// Providers: media answers).
func TestSpeechBatchItemErrorIsTheGateways(t *testing.T) {
	g := newTestGateway(t)
	withSpeechModels(t, g)
	g.backend.SetReply(fakebackend.Reply{FailItems: []int{1}})
	w := do(t, g.h, batchCall(`{"model":"tts","items":[{"input":"one"},{"input":"two"},{"input":"three"}]}`, "item-error"))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %.300s", w.Code, w.Body.String())
	}
	var answer struct {
		Results []struct {
			Index  int     `json:"index"`
			Status string  `json:"status"`
			Error  *string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || len(answer.Results) != 3 {
		t.Fatalf("answer %.300s (%v): want three results", w.Body.String(), err)
	}
	for i, r := range answer.Results {
		wantError := i == 1
		if r.Index != i || (r.Status == "error") != wantError || (r.Error != nil) != wantError {
			t.Errorf("result %d: index %d, status %q, error %v; want only the second errored", i, r.Index, r.Status, r.Error)
		}
	}
	if e := answer.Results[1].Error; e == nil || *e != "The model backend could not generate this item." {
		t.Errorf("the errored item's error %v, want the gateway's text", e)
	}
	for _, text := range []string{fakebackend.SpeechBatchItemError, "CUDA", "Qwen3-TTS-backend"} {
		if strings.Contains(w.Body.String(), text) {
			t.Errorf("the client got the backend's text %q", text)
		}
		if strings.Contains(g.logText(), text) {
			t.Errorf("the log holds the backend's text %q", text)
		}
	}
	expectUnits(t, onlyRecord(t, g), speechUnits(14, 8, 5), false, false)
}
