package server

import (
	"context"
	"encoding/binary"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
	"kaiak/internal/provider"
)

// withTranscriptionModels swaps in the test config plus speech-to-text models on the
// fake backend, then applies edits: "whisper" on a vllm backend "vl" (as
// "openai/whisper-large-v3"), with an output limit — which speech-to-text never
// writes — and "asr-ls" on a llama-server backend "ls" (as "qwen3-asr.gguf"), each
// listing audio_transcriptions alone; "chat-vl" on "vl" listing chat_completions alone,
// and "omni-tts" on a vllm-omni backend "omni" listing audio_speech. The fake's models
// list names whisper's backend-side model, which vLLM's model check reads.
func withTranscriptionModels(t *testing.T, g *testGateway, edits ...func(doc string) string) {
	t.Helper()
	g.backend.SetModels("openai/whisper-large-v3")
	g.apply(t, func(doc string) string {
		doc = replaceOnce(t, doc, `"backends": {`, `"backends": {
    "vl": { "type": "vllm", "base_url": "`+g.backend.URL()+`/v1" },
    "ls": { "type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" },
    "omni": { "type": "vllm-omni", "base_url": "`+g.backend.URL()+`/v1" },`)
		model := func(name, backend, backendModel, endpoints, extra string) string {
			return `"` + name + `": { "deployments": [{ "backend": "` + backend + `", "model": "` + backendModel + `" }],
      "endpoints": ` + endpoints + `,
      "metadata": { "context_length": 8192,
        "capabilities": { "streaming": false, "tools": false, "vision": false, "reasoning": false } }` + extra + ` },
    `
		}
		doc = replaceOnce(t, doc, `"models": { `, `"models": {
    `+model("whisper", "vl", "openai/whisper-large-v3", `["audio_transcriptions"]`, `, "output_limit": { "default": 50, "ceiling": 100 }`)+
			model("asr-ls", "ls", "qwen3-asr.gguf", `["audio_transcriptions"]`, "")+
			model("chat-vl", "vl", "openai/whisper-large-v3", `["chat_completions"]`, "")+
			model("omni-tts", "omni", "Qwen/Qwen3-TTS", `["audio_speech"]`, ""))
		for _, edit := range edits {
			doc = edit(doc)
		}
		return doc
	})
}

// transcriptionCall is a transcription of the eval group's key carrying form, a form
// body under testBoundary, request ID id.
func transcriptionCall(form, id string) call {
	return call{method: "POST", path: "/v1/audio/transcriptions", key: workloadKey, body: form,
		header: map[string]string{"X-Request-Id": id, "Content-Type": formContentType}}
}

// transcriptionUnits is a transcription record's units: its tokens and seconds of audio
// processed.
func transcriptionUnits(in, out, seconds int64) accounting.Units {
	u := units(in, 0, 0, out, 0)
	u[config.UnitAudioSecondsIn] = seconds
	return u
}

// wavData is an audio file part's bytes: a WAV file of 8-bit mono PCM at 32 Hz whose
// data is every byte value, CR and LF among them — 256 bytes, wavSeconds long.
var wavData = func() string {
	var b strings.Builder
	b.WriteString("RIFF\x24\x01\x00\x00WAVEfmt \x10\x00\x00\x00")
	// PCM, 1 channel, 32 Hz, 32 bytes a second, 1-byte blocks, 8 bits a sample.
	b.WriteString("\x01\x00\x01\x00\x20\x00\x00\x00\x20\x00\x00\x00\x01\x00\x08\x00")
	b.WriteString("data\x00\x01\x00\x00")
	for c := range 256 {
		b.WriteByte(byte(c))
	}
	return b.String()
}()

// wavSeconds is wavData's length: 256 bytes at 32 bytes a second.
const wavSeconds = 8

// A transcription passes the whole pipeline to a vllm and a llama-server deployment:
// its form rebuilt with the deployment's model — every other part as the client wrote
// it, timestamp_granularities[] repeated, a stream part saying false, an output-limit
// part and an unknown one included, no output limit of the gateway's added though the
// model declares one — answered with the backend's JSON as sent, its content type
// kept, never a stream. vLLM's answer settles its seconds and the prompt's estimate,
// flagged estimated; llama-server's its tokens — the prompt tokens it read from its
// cache as cache reads, out of its input — and the upload's measured seconds, nothing
// estimated. The token limits
// reserve the input estimate alone; operation generate_content and output type text on
// the record, the request line and the usage metrics, the seconds on the audio-input
// counter; never timed to a first token (docs/specs/GATEWAY.md, Client API → owned
// parts: speech-to-text; Request pipeline → Uploads; Limits → The input estimate,
// Output-limit keys; Accounting → Media usage: speech-to-text; Observability).
func TestTranscriptionThroughThePipeline(t *testing.T) {
	const prompt = "Kaiak, a gateway"
	estimate := accounting.EstimateUploadInput(int64(len(prompt)), 0).Total
	for _, c := range []struct {
		shape                            fakebackend.TranscriptionShape
		model, backendModel, backendType string
		limitPart                        rawPart
		// cached is the prompt tokens the backend reports read from its cache.
		cached    int64
		answer    string
		want      accounting.Units
		estimated bool
	}{
		{fakebackend.VLLMTranscription, "whisper", "openai/whisper-large-v3", "vllm", textPart("max_completion_tokens", "500"), 0,
			`{"text":"Hello from the fake","usage":{"seconds":3,"type":"duration"}}`, transcriptionUnits(estimate, 0, 3), true},
		{fakebackend.LlamaServerTranscription, "asr-ls", "qwen3-asr.gguf", "llama-server", textPart("max_tokens", "500"), 5,
			`{"text":"Hello from the fake","type":"transcript.text.done","usage":{"input_tokens":7,"input_tokens_details":` +
				`{"cached_tokens":5},"output_tokens":4,"total_tokens":11,"type":"tokens"}}`,
			transcriptionUnits(2, 4, wavSeconds), false},
	} {
		t.Run(c.backendType, func(t *testing.T) {
			g := newTestGateway(t)
			withTranscriptionModels(t, g, func(doc string) string {
				return replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "tokens_per_minute", "value": 100000 }] }`)
			})
			g.backend.SetTranscriptionShape(c.shape)
			g.backend.SetReply(fakebackend.Reply{CachedTokens: int(c.cached)})
			form := formOf(textPart("model", c.model), filePart("file", "voice memo ünï.wav", "audio/wav", wavData),
				textPart("prompt", prompt), textPart("language", "en"), textPart("timestamp_granularities[]", "word"),
				textPart("timestamp_granularities[]", "segment"), textPart("stream", "False"), textPart("response_format", "json"),
				textPart("temperature", "0"), c.limitPart, textPart("x_custom", "kept"))
			w := do(t, g.h, transcriptionCall(form, "stt-1"))
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("%d %s: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
			}
			if got := strings.TrimSuffix(w.Body.String(), "\n"); got != c.answer {
				t.Errorf("answer %s, want the backend's %s", got, c.answer)
			}
			if w.Header().Get("X-Accel-Buffering") != "" {
				t.Error("a transcription's answer was flushed as it arrived")
			}

			reqs := g.backend.Requests()
			if len(reqs) != 1 || reqs[0].Path != "/v1/audio/transcriptions" {
				t.Fatalf("backend requests %+v, want one to /v1/audio/transcriptions", reqs)
			}
			expectRebuilt(t, reqs[0], form, c.backendModel)

			rec := onlyRecord(t, g)
			wantUnits := maps.Clone(c.want)
			wantUnits[config.UnitTokensCached] = c.cached
			expectUnits(t, rec, wantUnits, c.estimated, false)
			if got := w.Header().Get("x-ratelimit-remaining-tokens"); got != strconv.FormatInt(100000-estimate, 10) {
				t.Errorf("remaining tokens at admission %s, want the input estimate of %d reserved alone", got, estimate)
			}
			if rec.Operation != "generate_content" || rec.OutputType != "text" {
				t.Errorf("record operation %q, output type %q; want generate_content, text", rec.Operation, rec.OutputType)
			}
			line := logLine(t, g, "stt-1")
			seconds := strconv.FormatInt(wantUnits[config.UnitAudioSecondsIn], 10)
			for _, want := range []string{`"gen_ai.operation.name":"generate_content"`, `"gen_ai.output.type":"text"`,
				`"gen_ai.request.stream":false`, `"kaiak.backend.type":"` + c.backendType + `"`,
				`"kaiak.usage.estimated":` + strconv.FormatBool(c.estimated),
				`"gen_ai.usage.input_tokens":` + strconv.FormatInt(wantUnits.Sum(config.InputUnits), 10),
				`"gen_ai.usage.cache_read.input_tokens":` + strconv.FormatInt(c.cached, 10),
				`"kaiak.usage.audio.input.duration":` + seconds} {
				if !strings.Contains(line, want) {
					t.Errorf("log line lacks %s: %s", want, line)
				}
			}
			usage := `{kaiak_key_group="eval",kaiak_key_root_group="research",kaiak_key_id="k-eval",gen_ai_request_model="` +
				c.model + `",gen_ai_operation_name="generate_content",gen_ai_output_type="text",kaiak_usage_status="complete"}`
			text := g.metricsText()
			expectMetricLines(t, text,
				`http_server_request_duration_seconds_count{http_request_method="POST",url_scheme="http",`+
					`http_route="/v1/audio/transcriptions",http_response_status_code="200",gen_ai_request_model="`+c.model+`"} 1`,
				`kaiak_usage_records_total`+usage+` 1`,
				`kaiak_usage_audio_input_duration_seconds_total`+usage+` `+seconds)
			if strings.Contains(text, `kaiak_time_to_first_token_seconds_count{gen_ai_request_model="`+c.model+`"`) {
				t.Error("a transcription was timed to a first token")
			}
		})
	}
}

// vLLM's seconds by response_format: json and text — JSON too — and diarized_json
// report them in their usage; verbose_json in its duration, rounded up to a whole
// second; llama-server's json reports tokens, and its seconds are the upload's. Each answer is relayed as the backend sent it, response_format passed as the
// client wrote it; the formats a server cannot answer — vLLM's srt and vtt,
// llama-server's anything but json — are the backend's to refuse: its 400 relayed, no
// units settled (docs/specs/GATEWAY.md, Client API → owned parts: speech-to-text;
// Accounting → Media usage: speech-to-text).
func TestTranscriptionResponseFormats(t *testing.T) {
	for _, c := range []struct {
		shape          fakebackend.TranscriptionShape
		model, format  string
		millis         int
		status         int
		answerContains string
		want           accounting.Units
	}{
		{fakebackend.VLLMTranscription, "whisper", "json", 2500, 200, `"seconds":3`, transcriptionUnits(0, 0, 3)},
		{fakebackend.VLLMTranscription, "whisper", "text", 4000, 200, `"seconds":4`, transcriptionUnits(0, 0, 4)},
		{fakebackend.VLLMTranscription, "whisper", "diarized_json", 61001, 200, `"seconds":62`, transcriptionUnits(0, 0, 62)},
		{fakebackend.VLLMTranscription, "whisper", "verbose_json", 2500, 200, `"duration":2.5`, transcriptionUnits(0, 0, 3)},
		{fakebackend.VLLMTranscription, "whisper", "verbose_json", 4000, 200, `"duration":4`, transcriptionUnits(0, 0, 4)},
		{fakebackend.VLLMTranscription, "whisper", "srt", 2500, 400, "srt", transcriptionUnits(0, 0, 0)},
		{fakebackend.VLLMTranscription, "whisper", "vtt", 2500, 400, "vtt", transcriptionUnits(0, 0, 0)},
		{fakebackend.LlamaServerTranscription, "asr-ls", "json", 2500, 200, `"input_tokens":7`, transcriptionUnits(7, 4, wavSeconds)},
		{fakebackend.LlamaServerTranscription, "asr-ls", "text", 2500, 400, "text", transcriptionUnits(0, 0, 0)},
		{fakebackend.LlamaServerTranscription, "asr-ls", "verbose_json", 2500, 400, "verbose_json", transcriptionUnits(0, 0, 0)},
	} {
		t.Run(c.model+"/"+c.format, func(t *testing.T) {
			g := newTestGateway(t)
			withTranscriptionModels(t, g)
			g.backend.SetTranscriptionShape(c.shape)
			g.backend.SetReply(fakebackend.Reply{AudioMillis: c.millis})
			form := formOf(textPart("model", c.model), filePart("file", "a.wav", "audio/wav", wavData),
				textPart("response_format", c.format))
			w := do(t, g.h, transcriptionCall(form, "fmt"))
			if w.Code != c.status || !strings.Contains(w.Body.String(), c.answerContains) {
				t.Fatalf("%d %s, want %d holding %s", w.Code, w.Body.String(), c.status, c.answerContains)
			}
			expectRebuilt(t, g.backend.Requests()[0], form, map[string]string{"whisper": "openai/whisper-large-v3",
				"asr-ls": "qwen3-asr.gguf"}[c.model])
			// vLLM's tokens are estimated: no prompt, an estimate of nothing; llama-server's
			// are reported, its seconds measured.
			expectUnits(t, onlyRecord(t, g), c.want, c.status == http.StatusOK && c.shape == fakebackend.VLLMTranscription, false)
		})
	}
}

// flacData is a FLAC stream declaring total samples at rate, mono 16-bit: STREAMINFO
// alone, then a frame's sync and filler, which nothing reads.
func flacData(rate, total uint64) string {
	info := []byte{0x10, 0x00, 0x10, 0x00, 0, 0, 0, 0, 0, 0}
	info = binary.BigEndian.AppendUint64(info, rate<<44|15<<36|total)
	info = append(info, make([]byte, 16)...)
	return "fLaC\x80\x00\x00\x22" + string(info) + "\xff\xf8" + strings.Repeat("\x00", 64)
}

// mp3Data is n frames of MPEG-1 Layer III, 32 kbit/s mono at 48 kHz: 96 bytes and 1152
// samples each.
func mp3Data(n int) string {
	return strings.Repeat("\xff\xfb\x14\xc0"+strings.Repeat("\x00", 92), n)
}

// m4aData is the opening of an M4A file, a format llama-server does not decode.
const m4aData = "\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x08free"

// A llama-server transcription counts its upload's measured length, rounded up to a
// whole second — WAV, FLAC and MP3, told apart by their bytes, never by the filename
// or the content type the client gives them — with the answer's tokens, nothing
// estimated (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).
func TestLlamaServerTranscriptionCountsTheMeasuredUpload(t *testing.T) {
	for _, c := range []struct {
		name, filename, contentType, file string
		seconds                           int64
	}{
		{"WAV", "memo.wav", "audio/wav", wavData, wavSeconds},
		{"WAV named an MP3", "memo.mp3", "audio/mpeg", wavData, wavSeconds},
		{"FLAC, 30.5 s", "memo.flac", "audio/flac", flacData(16000, 16000*61/2), 31},
		{"MP3, 2.4 s, named a WAV", "memo.wav", "audio/wav", mp3Data(100), 3},
		{"MP3 of 125 frames, 3 s", "memo.mp3", "application/octet-stream", mp3Data(125), 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withTranscriptionModels(t, g)
			g.backend.SetTranscriptionShape(fakebackend.LlamaServerTranscription)
			form := formOf(textPart("model", "asr-ls"), filePart("file", c.filename, c.contentType, c.file))
			if w := do(t, g.h, transcriptionCall(form, "measured")); w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			expectRebuilt(t, g.backend.Requests()[0], form, "qwen3-asr.gguf")
			expectUnits(t, onlyRecord(t, g), transcriptionUnits(7, 4, c.seconds), false, false)
		})
	}
}

// An upload the gateway cannot measure, sent to a llama-server model, is refused 400
// audio_format_unsupported, naming the file part — never its filename or its bytes —
// once the model's type is known: after the endpoints list, so a llama-server model
// that does not list speech-to-text answers endpoint_not_served; before limits, so the
// group's one request a minute is still free for a measurable upload after the
// refusals; before any backend, and with no usage record (docs/specs/GATEWAY.md,
// Request pipeline, stage 2; Accounting → Media usage: speech-to-text).
func TestUnmeasurableUploadRefusedOnLlamaServer(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g, func(doc string) string {
		doc = replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "requests_per_minute", "value": 1 }] }`)
		return replaceOnce(t, doc, `"models": {`, `"models": {
    "chat-ls": { "deployments": [{ "backend": "ls", "model": "qwen3-asr.gguf" }], "endpoints": ["chat_completions"],
      "metadata": { "context_length": 8192, "capabilities": { "streaming": false, "tools": false, "vision": false, "reasoning": false } } },`)
	})
	g.backend.SetTranscriptionShape(fakebackend.LlamaServerTranscription)
	upload := func(model, file string) string {
		return formOf(textPart("model", model), filePart("file", "secret-recording.wav", "audio/wav", file))
	}
	for name, file := range map[string]string{
		"M4A":                       m4aData,
		"Ogg":                       "OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00",
		"text":                      "not audio at all",
		"a frame sync and no frame": "\xff\xfbno frame follows",
		"an empty file":             "",
		"a WAV header cut short":    wavData[:30],
		"an ID3 tag and no frame":   "ID3\x04\x00\x00\x00\x00\x00\x02ab",
		"a FLAC of unknown length":  flacData(16000, 0),
	} {
		w := do(t, g.h, transcriptionCall(upload("asr-ls", file), "unmeasurable"))
		expectError(t, w, http.StatusBadRequest, "audio_format_unsupported")
		if _, _, param := openAIError(t, w); param == nil || *param != "file" {
			t.Errorf("%s: param %v, want file", name, param)
		}
		if msg := errorMessage(t, w); strings.Contains(msg, "secret-recording") || strings.Contains(msg, "audio/wav") {
			t.Errorf("%s: message %q names the client's file", name, msg)
		}
	}
	expectError(t, do(t, g.h, transcriptionCall(upload("chat-ls", m4aData), "unlisted")), http.StatusBadRequest,
		"endpoint_not_served")
	if n, u := len(g.backend.Requests()), len(g.usage.all()); n != 0 || u != 0 {
		t.Fatalf("%d backend requests and %d usage records, want none", n, u)
	}
	if w := do(t, g.h, transcriptionCall(upload("asr-ls", wavData), "measurable")); w.Code != http.StatusOK {
		t.Fatalf("a measurable upload after the refusals: %d %s, want the request slot free", w.Code, w.Body.String())
	}
	expectError(t, do(t, g.h, transcriptionCall(upload("asr-ls", wavData), "spent")), http.StatusTooManyRequests,
		"rate_limit_exceeded")
}

// A vllm transcription is not refused for an upload the gateway cannot measure, and
// its answer's seconds win over a measured upload's. An answer reporting none — none
// is expected — counts the measured upload, else 0; the record is estimated either
// way, as its tokens are (docs/specs/GATEWAY.md, Accounting → Media usage:
// speech-to-text).
func TestVLLMTranscriptionAndTheMeasuredUpload(t *testing.T) {
	for _, c := range []struct {
		name      string
		file      string
		omitUsage bool
		seconds   int64
	}{
		{"measured, the answer's seconds", wavData, false, 3},
		{"not measured, the answer's seconds", m4aData, false, 3},
		{"measured, no seconds in the answer", wavData, true, wavSeconds},
		{"not measured, no seconds in the answer", m4aData, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newTestGateway(t)
			withTranscriptionModels(t, g)
			g.backend.SetReply(fakebackend.Reply{OmitUsage: c.omitUsage})
			form := formOf(textPart("model", "whisper"), filePart("file", "memo.m4a", "audio/mp4", c.file))
			if w := do(t, g.h, transcriptionCall(form, "vllm")); w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			expectUnits(t, onlyRecord(t, g), transcriptionUnits(0, 0, c.seconds), true, false)
		})
	}
}

// What the gateway refuses on /v1/audio/transcriptions it refuses before routing, in
// OpenAI's error shape, naming the part at fault and never echoing its value: no file
// part, a file given as text, two files; a stream part other than false; vllm_xargs,
// whatever it holds; an n above max_n (8 here), or not a whole number; an owned text
// part sent as a file; a single-valued part named twice; no model; OpenAI's JSON body;
// a model whose endpoints list leaves audio_transcriptions out — on vllm, on
// vllm-omni, on openai-compatible (docs/specs/GATEWAY.md, Client API → owned parts:
// speech-to-text; errors; Limits → Output multiplicity; Model metadata → Endpoints per
// model).
func TestTranscriptionRefusals(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g)
	transcription := func(parts ...rawPart) string {
		return formOf(append([]rawPart{textPart("model", "whisper"), filePart("file", "a.wav", "audio/wav", "RIFF")}, parts...)...)
	}
	cases := []struct {
		name, form, code, param string
	}{
		{"no file", formOf(textPart("model", "whisper"), textPart("prompt", "hi")), "missing_required_parameter", "file"},
		{"a file given as text", formOf(textPart("model", "whisper"), textPart("file", "RIFF-as-text")), "invalid_type", "file"},
		{"two files", transcription(filePart("file", "b.wav", "audio/wav", "RIFF")), "duplicate_member", "file"},
		{"a file as text after one", transcription(textPart("file", "RIFF-as-text")), "duplicate_member", "file"},
		{"stream a file", transcription(filePart("stream", "s.txt", "text/plain", "false")), "invalid_type", "stream"},
		{"vllm_xargs", transcription(textPart("vllm_xargs", `{"lora_path":"/srv/adapters/persimmon"}`)),
			"external_reference_unsupported", "vllm_xargs"},
		{"vllm_xargs empty", transcription(textPart("vllm_xargs", "")), "external_reference_unsupported", "vllm_xargs"},
		{"vllm_xargs a file", transcription(filePart("vllm_xargs", "x.json", "application/json", `{"path":"/srv/adapters"}`)),
			"external_reference_unsupported", "vllm_xargs"},
		{"response_format a file", transcription(filePart("response_format", "f.txt", "text/plain", "verbose_json")),
			"invalid_type", "response_format"},
		{"n above max_n", transcription(textPart("n", "9")), "n_too_large", "n"},
		{"n not a whole number", transcription(textPart("n", "2.0")), "invalid_type", "n"},
		{"n past int64", transcription(textPart("n", "9223372036854775808")), "invalid_type", "n"},
		{"n a file", transcription(filePart("n", "n.txt", "text/plain", "2")), "invalid_type", "n"},
		{"n named twice", transcription(textPart("n", "1"), textPart("n", "2")), "duplicate_member", "n"},
		{"response_format named twice", transcription(textPart("response_format", "json"), textPart("response_format", "srt")),
			"duplicate_member", "response_format"},
		{"language named twice", transcription(textPart("language", "en"), textPart("language", "fr")), "duplicate_member", "language"},
		{"model missing", formOf(filePart("file", "a.wav", "audio/wav", "RIFF")), "missing_required_parameter", "model"},
		{"not listed on a vllm model", formOf(textPart("model", "chat-vl"), filePart("file", "a.wav", "audio/wav", "RIFF")),
			"endpoint_not_served", ""},
		{"not on vllm-omni", formOf(textPart("model", "omni-tts"), filePart("file", "a.wav", "audio/wav", "RIFF")),
			"endpoint_not_served", ""},
		{"not on openai-compatible", formOf(textPart("model", "open"), filePart("file", "a.wav", "audio/wav", "RIFF")),
			"endpoint_not_served", ""},
	}
	for _, value := range []string{"true", "TRUE", "1", "on", "yes", ""} {
		cases = append(cases, struct{ name, form, code, param string }{"stream " + strconv.Quote(value),
			transcription(textPart("stream", value)), "invalid_value", "stream"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, g.h, transcriptionCall(c.form, "refused"))
			expectError(t, w, http.StatusBadRequest, c.code)
			if _, _, param := openAIError(t, w); (param == nil && c.param != "") || (param != nil && *param != c.param) {
				t.Errorf("param %v, want %q: %s", param, c.param, w.Body.String())
			}
			msg := errorMessage(t, w)
			for _, value := range []string{"/srv/", "persimmon", "RIFF", "verbose_json", "lora_path"} {
				if strings.Contains(msg, value) {
					t.Errorf("message %q echoes the value", msg)
				}
			}
			if c.code == "endpoint_not_served" && msg != notListed("/v1/audio/transcriptions") {
				t.Errorf("message %q, want the list's refusal", msg)
			}
		})
	}

	// OpenAI's JSON body is refused before it is read.
	w := do(t, g.h, call{method: "POST", path: "/v1/audio/transcriptions", key: workloadKey,
		body: `{"model":"whisper","file":"UklGRg=="}`, header: map[string]string{"Content-Type": "application/json"}})
	expectError(t, w, http.StatusBadRequest, "unsupported_content_type")

	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
	if n := len(g.usage.all()); n != 0 {
		t.Errorf("%d usage records, want none: nothing was routed", n)
	}
}

// A transcription's parts as the inbound stage reads them: it counts toward the
// audio-processed limits and reserves nothing there; its meter is told whether it asked
// for verbose_json — that exact name; it is never a stream; its sequences are its n,
// at most max_n, below 1 counted as 1, absent 1; its input estimate is its prompt's
// text, a prompt sent as a file part none (docs/specs/GATEWAY.md, Client API → owned
// parts: speech-to-text; Limits → Check and reserve, The input estimate, Output
// multiplicity).
func TestTranscriptionReadsItsParts(t *testing.T) {
	ep := bodyEndpoint(provider.AudioTranscriptions)
	for _, c := range []struct {
		name      string
		parts     []rawPart
		verbose   bool
		prompt    int64
		sequences int64
	}{
		{"no response_format", nil, false, 0, 1},
		{"json", []rawPart{textPart("response_format", "json")}, false, 0, 1},
		{"verbose_json", []rawPart{textPart("response_format", "verbose_json")}, true, 0, 1},
		{"verbose_json in another case", []rawPart{textPart("response_format", "Verbose_JSON")}, false, 0, 1},
		{"a prompt", []rawPart{textPart("prompt", "Kaiak and its gateway")}, false, 21, 1},
		{"a prompt as a file", []rawPart{filePart("prompt", "p.txt", "text/plain", "Kaiak and its gateway")}, false, 0, 1},
		{"a stream part saying false", []rawPart{textPart("stream", "fAlSe")}, false, 0, 1},
		{"n at max_n", []rawPart{textPart("n", "8")}, false, 0, 8},
		{"n of 3, signed", []rawPart{textPart("n", "+3")}, false, 0, 3},
		{"n of 0", []rawPart{textPart("n", "0")}, false, 0, 1},
		{"n below 0", []rawPart{textPart("n", "-4")}, false, 0, 1},
	} {
		parts := append([]rawPart{textPart("model", "whisper"), filePart("file", "a.wav", "audio/wav", "RIFF")}, c.parts...)
		rq, _, apiErr := readTestUpload(ep, uploadRequest(strings.NewReader(formOf(parts...)), formContentType), uploadSnapshot(t, 0),
			NewBodyBudget(DefaultBodyMemory), testUploads(DefaultUploadMemory))
		if apiErr != nil {
			t.Fatalf("%s: refused %+v", c.name, apiErr)
		}
		if got := rq.inbound.Media; !maps.Equal(got, map[config.Measure]int64{config.MeasureAudioSecondsIn: 0}) {
			t.Errorf("%s: media %v, want audio processed, nothing reserved", c.name, got)
		}
		if got := rq.inbound.mediaRequest().Transcription; got != (accounting.Transcription{VerboseJSON: c.verbose}) ||
			rq.inbound.Stream {
			t.Errorf("%s: the meter told %+v, stream %v; want verbose_json %v, not a stream", c.name, got, rq.inbound.Stream,
				c.verbose)
		}
		if want := accounting.EstimateUploadInput(c.prompt, 0); rq.input != want {
			t.Errorf("%s: estimate %+v, want %+v: the prompt's %d bytes", c.name, rq.input, want, c.prompt)
		}
		if rq.inbound.Sequences != c.sequences {
			t.Errorf("%s: %d sequences, want %d", c.name, rq.inbound.Sequences, c.sequences)
		}
		rq.finish()
	}
}

// A prompt part with an empty filename is refused, 400 invalid_body, before any
// backend: cpp-httplib reads a part with an empty filename as a text field, so
// llama-server would take it as the transcription's prompt while the gateway read it
// as a file — left out of the input estimate the token limits admit the request by,
// and past the text part's cap. The same prompt as a text part counts: over the
// group's 100 tokens a minute, 429 (docs/specs/GATEWAY.md, Request pipeline → Uploads;
// Limits → The input estimate).
func TestTranscriptionPromptWithAnEmptyFilenameIsRefused(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "tokens_per_minute", "value": 100 }] }`)
	})
	g.backend.SetTranscriptionShape(fakebackend.LlamaServerTranscription)
	prompt := strings.Repeat("word ", 400)
	parts := []rawPart{textPart("model", "asr-ls"), filePart("file", "audio.wav", "audio/wav", wavData)}
	counted := do(t, g.h, transcriptionCall(formOf(append(parts, textPart("prompt", prompt))...), "prompt-as-text"))
	expectError(t, counted, http.StatusTooManyRequests, "rate_limit_exceeded")
	hidden := do(t, g.h, transcriptionCall(formOf(append(parts, filePart("prompt", "", "text/plain", prompt))...),
		"prompt-with-an-empty-filename"))
	expectError(t, hidden, http.StatusBadRequest, "invalid_body")
	if n := len(g.backend.Requests()); n != 0 {
		t.Errorf("%d requests reached the backend, want none", n)
	}
}

// A transcription sent but never answered — its backend silent past the response
// timeout, 504 upstream_timeout — records 0 seconds of audio processed, as an
// unanswered image request records 0 images: the input estimate is billed as sent, no
// output, estimated and partial. On llama-server too, whose upload the gateway
// measured: the measured length counts only for an answer (docs/specs/GATEWAY.md,
// Accounting → Billed from the moment the request was sent; Media usage:
// speech-to-text).
func TestUnansweredTranscriptionRecordsNoAudio(t *testing.T) {
	const prompt = "Kaiak"
	estimate := accounting.EstimateUploadInput(int64(len(prompt)), 0).Total
	for _, c := range []struct {
		model string
		shape fakebackend.TranscriptionShape
	}{
		{"whisper", fakebackend.VLLMTranscription},
		{"asr-ls", fakebackend.LlamaServerTranscription},
	} {
		t.Run(c.model, func(t *testing.T) {
			g := newTestGateway(t)
			withTranscriptionModels(t, g, func(doc string) string {
				doc = replaceOnce(t, doc, `"type": "vllm", "base_url": "`+g.backend.URL()+`/v1" }`,
					`"type": "vllm", "base_url": "`+g.backend.URL()+`/v1", "response_timeout_ms": 150 }`)
				return replaceOnce(t, doc, `"type": "llama-server", "base_url": "`+g.backend.URL()+`/v1" }`,
					`"type": "llama-server", "base_url": "`+g.backend.URL()+`/v1", "response_timeout_ms": 150 }`)
			})
			g.backend.SetTranscriptionShape(c.shape)
			g.backend.SetReply(fakebackend.Reply{Before: fakebackend.StallFirstByte})
			form := formOf(textPart("model", c.model), filePart("file", "a.wav", "audio/wav", wavData), textPart("prompt", prompt))
			expectError(t, do(t, g.h, transcriptionCall(form, "unanswered")), http.StatusGatewayTimeout, "upstream_timeout")
			expectUnits(t, onlyRecord(t, g), transcriptionUnits(estimate, 0, 0), true, true)
		})
	}
}

// The audio-processed limits reserve nothing for a transcription — its seconds come
// from its answer — and settle to its record: the window admits transcriptions while
// it is below its value, a running one can take it past, and once it has reached its
// value the next transcription is refused, 429 audio_seconds, on either server. Chat,
// which processes no audio, passes (docs/specs/GATEWAY.md, Limits → Check and reserve,
// Settle).
func TestTranscriptionLimits(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g, func(doc string) string {
		return replaceOnce(t, doc, `"research": { "capture": false }`, `"research": { "capture": false, "limits": [{ "type": "audio_seconds_in_per_minute", "value": 5 }] }`)
	})
	transcription := func(model string) string {
		return formOf(textPart("model", model), filePart("file", "a.wav", "audio/wav", wavData))
	}
	if w := do(t, g.h, transcriptionCall(transcription("whisper"), "first")); w.Code != http.StatusOK {
		t.Fatalf("a transcription under the limit: %d %s", w.Code, w.Body.String())
	}
	if used := counterUsed(t, g, "research", config.LimitAudioSecondsInPerMinute); used != 3 {
		t.Errorf("window holds %d seconds, want the answer's 3", used)
	}
	if w := do(t, g.h, transcriptionCall(transcription("whisper"), "second")); w.Code != http.StatusOK {
		t.Fatalf("a transcription with 2 of 5 seconds left: %d %s, want it admitted: nothing is reserved", w.Code, w.Body.String())
	}
	if used := counterUsed(t, g, "research", config.LimitAudioSecondsInPerMinute); used != 6 {
		t.Errorf("window holds %d seconds, want 6: the second answer took it past its value", used)
	}
	sent := len(g.backend.Requests())
	for _, model := range []string{"whisper", "asr-ls"} {
		w := do(t, g.h, transcriptionCall(transcription(model), "full-"+model))
		expectError(t, w, http.StatusTooManyRequests, "rate_limit_exceeded")
		if typ, _, _ := openAIError(t, w); typ != "audio_seconds" || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s: type %q, Retry-After %q: want audio_seconds and a wait", model, typ, w.Header().Get("Retry-After"))
		}
		if msg := errorMessage(t, w); !strings.Contains(msg, "limit of 5 seconds of audio processed per minute (used 6)") {
			t.Errorf("%s: message %q", model, msg)
		}
	}
	if n := len(g.backend.Requests()); n != sent {
		t.Errorf("%d refused transcriptions reached the backend", n-sent)
	}
	if w := do(t, g.h, call{method: "POST", path: "/v1/chat/completions", key: workloadKey, body: chatBody}); w.Code != http.StatusOK {
		t.Errorf("chat refused by the audio-processed limit: %d %s", w.Code, w.Body.String())
	}
}

// A transcription's answer cut short before its usage settles as one that reported no
// seconds — the measured upload's — with the input estimate, flagged estimated and
// partial; the client's connection is cut (docs/specs/GATEWAY.md, Accounting →
// Partial).
func TestTranscriptionCutShort(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g)
	g.backend.SetReply(fakebackend.Reply{Fault: &fakebackend.StreamFault{Kind: fakebackend.Cut}})
	const prompt = "Kaiak"
	form := formOf(textPart("model", "whisper"), filePart("file", "a.wav", "audio/wav", wavData), textPart("prompt", prompt))
	req, err := http.NewRequestWithContext(context.Background(), "POST", serveGateway(t, g)+"/v1/audio/transcriptions",
		strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+workloadKey)
	req.Header.Set("Content-Type", formContentType)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err == nil {
			t.Errorf("status %d, read error %v: want the answer cut short", resp.StatusCode, err)
		}
	}
	expectUnits(t, settledRecord(t, g), transcriptionUnits(accounting.EstimateUploadInput(int64(len(prompt)), 0).Total, 0,
		wavSeconds), true, true)
}

// The servers' own answers on a transcription: vLLM's model check naming the
// deployment's model is the model missing there — 502 upstream_model_missing, its
// text not relayed, no units; a model that does not hear audio is the server's to
// answer — vLLM mounts no route (502 upstream_path_missing, the one "not found"
// reading), llama-server answers 501 not_supported_error, a backend 5xx answered as
// upstream_error under its status (docs/specs/GATEWAY.md, Providers → Wrong model on a
// host, A path the server does not have, Upstream failures).
func TestTranscriptionServerAnswers(t *testing.T) {
	transcription := func(model string) string {
		return formOf(textPart("model", model), filePart("file", "a.wav", "audio/wav", wavData))
	}
	g := newTestGateway(t)
	withTranscriptionModels(t, g)
	g.backend.SetModels("another/model")
	w := do(t, g.h, transcriptionCall(transcription("whisper"), "stale"))
	expectError(t, w, http.StatusBadGateway, "upstream_model_missing")
	if strings.Contains(w.Body.String(), "whisper-large") || strings.Contains(w.Body.String(), "does not exist") {
		t.Errorf("the backend's answer reached the client: %s", w.Body.String())
	}
	expectUnits(t, onlyRecord(t, g), transcriptionUnits(0, 0, 0), false, true)

	for _, c := range []struct {
		shape        fakebackend.TranscriptionShape
		model, back  string
		status       int
		code         string
		backendCalls int
	}{
		{fakebackend.VLLMTranscription, "whisper", "openai/whisper-large-v3", http.StatusBadGateway, "upstream_path_missing", 1},
		{fakebackend.LlamaServerTranscription, "asr-ls", "qwen3-asr.gguf", http.StatusNotImplemented, "upstream_error", 1},
	} {
		g := newTestGateway(t)
		withTranscriptionModels(t, g)
		g.backend.SetTranscriptionShape(c.shape)
		g.backend.SetNonAudioModels(c.back)
		w := do(t, g.h, transcriptionCall(transcription(c.model), "deaf"))
		expectError(t, w, c.status, c.code)
		if strings.Contains(w.Body.String(), "audio input") || strings.Contains(w.Body.String(), "Not Found") {
			t.Errorf("%s: the backend's text reached the client: %s", c.model, w.Body.String())
		}
		if n := len(g.backend.Requests()); n != c.backendCalls {
			t.Errorf("%s: %d backend requests, want %d: neither answer is retried", c.model, n, c.backendCalls)
		}
	}
}

// Nothing of a transcription's prompt, its filename, its audio, a vllm_xargs refused,
// its hotwords or the answer's text reaches the log (docs/specs/GATEWAY.md, Request
// pipeline → Uploads; Observability: logs).
func TestTranscriptionLogsHoldNoContent(t *testing.T) {
	g := newTestGateway(t)
	withTranscriptionModels(t, g)
	base := []rawPart{textPart("model", "whisper"), filePart("file", "tangerine-holiday.wav", "audio/wav", "RIFF pomegranate-samples"),
		textPart("prompt", "zanzibar quokka"), textPart("hotwords", "whisper marmalade")}
	if w := do(t, g.h, transcriptionCall(formOf(base...), "secret-1")); w.Code != http.StatusOK {
		t.Fatalf("%d %.200s", w.Code, w.Body.String())
	}
	do(t, g.h, transcriptionCall(formOf(append(base, textPart("vllm_xargs", `{"path":"/srv/persimmon"}`))...), "secret-2"))
	do(t, g.h, transcriptionCall(formOf(append(base, filePart("file", "durian-trip.wav", "audio/wav", "x"))...), "secret-3"))
	g.backend.SetModels("lychee/model")
	do(t, g.h, transcriptionCall(formOf(base...), "secret-4"))
	logs := g.logText()
	for _, secret := range []string{"zanzibar", "quokka", "marmalade", "tangerine", "pomegranate", "persimmon", "durian",
		"lychee", "Hello from the fake", "The model `"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
}
