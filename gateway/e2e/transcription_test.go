package e2e

// Speech-to-text end to end (docs/specs/GATEWAY.md, Client API → owned parts:
// speech-to-text; Request pipeline → Uploads; Accounting → Media usage: speech-to-text;
// Limits → Check and reserve; Providers → A path the server does not have): one
// gateway passes transcriptions — multipart/form-data uploads — to a vllm and a
// llama-server backend, each a fake backend answering in its server's shape. The
// backend gets the form rebuilt with the deployment's model, and its answer comes back
// as it sent it. A vllm request settles the seconds of audio processed its answer
// reports — usage.seconds, or a verbose_json answer's duration rounded up — whatever
// the upload's length, and its prompt's tokens estimated, flagged; a llama-server
// request settles the seconds the gateway measured from its upload — WAV, MP3 or FLAC
// — and the tokens the answer reports. An upload the gateway cannot measure is refused
// before a llama-server backend, as are a body that is no form, an upload over the cap
// and a transcription to a model whose list leaves speech-to-text out; an
// audio-processed limit counts after the request and refuses transcriptions once its
// window is spent. A wrong base_url on one of a model's two deployments is answered at
// once, never retried on the other, and never opens its circuit. Nothing of the
// prompt, the filenames or the text reaches the log.

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"kaiak/internal/accounting"
	"kaiak/internal/fakebackend"
)

// The backend-side models of the speech-to-text scenarios.
const (
	vllmASRModel  = "openai/whisper-large-v3"
	llamaASRModel = "qwen3-asr-1.7b-q8_0.gguf"
)

// transcriptionUploadCap is the speech-to-text scenario's global.max_upload_bytes: 64
// KiB, which every transcription it sends fits.
const transcriptionUploadCap = 64 << 10

// transcriptionText is what the fake backends transcribe: their default chunks joined.
const transcriptionText = "Hello from the fake"

// transcriptionConfig is a config with a vllm backend "vl" on the fake backend at
// vlURL and a llama-server backend "ls" on the one at lsURL: the models "whisper" (on
// vl as vllmASRModel) and "asr" (on ls as llamaASRModel), whose endpoints lists name
// speech-to-text alone, and "chat" (on a second vllm backend "vl-chat" at vlURL),
// chat alone; global.max_upload_bytes transcriptionUploadCap; the group w with the
// key k of hash, and the group listen (audio_seconds_in_per_minute 5) with the key
// k-listen of listenHash.
func transcriptionConfig(vlURL, lsURL, hash, listenHash string) map[string]any {
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
		"global":           map[string]any{"max_upload_bytes": transcriptionUploadCap},
		"backends": map[string]any{
			"vl":      map[string]any{"type": "vllm", "base_url": vlURL + "/v1"},
			"vl-chat": map[string]any{"type": "vllm", "base_url": vlURL + "/v1"},
			"ls":      map[string]any{"type": "llama-server", "base_url": lsURL + "/v1"},
		},
		"models": map[string]any{
			"whisper": model("vl", vllmASRModel, "audio_transcriptions"),
			"asr":     model("ls", llamaASRModel, "audio_transcriptions"),
			"chat":    model("vl-chat", backendChatModel, "chat_completions"),
		},
		"groups": map[string]any{
			"w": map[string]any{"capture": false, "allowed_models": []any{"*"}},
			"listen": map[string]any{"capture": false, "allowed_models": []any{"*"},
				"limits": []any{map[string]any{"type": "audio_seconds_in_per_minute", "value": 5}}},
		},
		"keys": map[string]any{
			"k":        map[string]any{"hash": hash, "group": "w"},
			"k-listen": map[string]any{"hash": listenHash, "group": "listen"},
		},
	}
}

// wavUpload is a WAV file as a client uploads it: 16-bit mono PCM at rate, its data
// dataBytes of silence — dataBytes / (2 × rate) seconds long.
func wavUpload(rate, dataBytes int) string { return string(wavClip(rate, dataBytes, false)) }

// mp3Upload is n frames of MPEG-1 Layer III at 32 kbit/s, mono, 48 kHz — 96 bytes and
// 1152 samples (24 ms) each, their audio silence.
func mp3Upload(n int) string { return strings.Repeat("\xff\xfb\x14\xc0"+strings.Repeat("\x00", 92), n) }

// flacUpload is a FLAC file declaring total samples at rate, 16-bit mono: the fLaC
// marker, its STREAMINFO block alone — block sizes 4096, frame sizes and MD5 unknown —
// then a frame's sync and filler, which nothing reads.
func flacUpload(rate, total uint64) string {
	info := []byte{0x10, 0x00, 0x10, 0x00, 0, 0, 0, 0, 0, 0}
	// Rate (20 bits), channels less 1 (3), bits per sample less 1 (5), total samples (36).
	info = binary.BigEndian.AppendUint64(info, rate<<44|15<<36|total)
	info = append(info, make([]byte, 16)...)
	return "fLaC\x80\x00\x00\x22" + string(info) + "\xff\xf8" + strings.Repeat("\x00", 64)
}

// m4aUpload is the opening of an M4A file, a format llama-server does not decode and
// the gateway does not measure.
const m4aUpload = "\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x08free"

// transcriptionAnswer is a transcription answer as the client gets it: its text and
// members, decoded.
func transcriptionAnswer(t *testing.T, r *response) map[string]any {
	t.Helper()
	if r.StatusCode != http.StatusOK || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d (%s) %.300s, want 200 with a JSON body", r.StatusCode, r.Header.Get("Content-Type"), r.body)
	}
	answer := r.json(t)
	if answer["text"] != transcriptionText {
		t.Errorf("answer %s: want the backend's text %q", r.body, transcriptionText)
	}
	return answer
}

func TestTranscription(t *testing.T) {
	vl, ls := fakebackend.New(), fakebackend.New()
	t.Cleanup(vl.Close)
	t.Cleanup(ls.Close)
	// vLLM checks a transcription's model against the names it serves.
	vl.SetModels(vllmASRModel, backendChatModel)
	ls.SetModels(llamaASRModel)
	ls.SetTranscriptionShape(fakebackend.LlamaServerTranscription)
	received := func() int { return len(vl.Requests()) + len(ls.Requests()) }
	key, hash := newKey()
	listenKey, listenHash := newKey()
	configFile := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, configFile, transcriptionConfig(vl.URL(), ls.URL(), hash, listenHash))
	g := startGateway(t, configFile)
	const prompt = "Kaiak, a gateway"
	promptEstimate := accounting.EstimateUploadInput(int64(len(prompt)), 0).Total

	// An 8 s WAV — 1000 Hz, 16000 bytes of data — on whisper: the backend gets the form
	// rebuilt, its list part repeated and the parts the gateway does not read as the
	// client wrote them; the request settles the seconds the answer reports, never the
	// upload's 8 — 2.5 s reported as 3 in usage.seconds, a verbose_json duration of 4.2
	// rounded up to 5 — and the prompt's tokens estimated, flagged, no output.
	for _, c := range []struct {
		format  string
		millis  int
		seconds int64
	}{
		{"json", 2500, 3},
		{"verbose_json", 4200, 5},
	} {
		t.Run("vllm/"+c.format, func(t *testing.T) {
			vl.SetReply(fakebackend.Reply{AudioMillis: c.millis})
			defer vl.SetReply(fakebackend.Reply{})
			id := "stt-vllm-" + c.format
			contentType, body := writeForm(t, textPart("model", "whisper"),
				filePart("file", "tangerine-holiday.wav", "audio/wav", wavUpload(1000, 16000)), textPart("prompt", prompt),
				textPart("language", "en"), textPart("timestamp_granularities[]", "word"),
				textPart("timestamp_granularities[]", "segment"), textPart("response_format", c.format),
				textPart("temperature", "0"))
			before := len(vl.Requests())
			answer := transcriptionAnswer(t, g.postForm(t, "/v1/audio/transcriptions", key, id, contentType, body))
			switch c.format {
			case "verbose_json":
				if answer["duration"] != float64(c.millis)/1000 {
					t.Errorf("answer %v: want the backend's duration %v", answer, float64(c.millis)/1000)
				}
			default:
				if usage, _ := answer["usage"].(map[string]any); usage["seconds"] != float64(c.seconds) {
					t.Errorf("answer %v: want the backend's usage.seconds %d", answer, c.seconds)
				}
			}

			reqs := vl.Requests()
			if len(reqs) != before+1 || reqs[len(reqs)-1].Path != "/v1/audio/transcriptions" {
				t.Fatalf("backend got %d requests, want 1 to /v1/audio/transcriptions", len(reqs)-before)
			}
			wantRebuilt(t, reqs[len(reqs)-1], contentType, body, vllmASRModel)
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.backend.id": "vl", "kaiak.backend.type": "vllm",
				"kaiak.deployment.model": vllmASRModel, "gen_ai.operation.name": "generate_content", "gen_ai.output.type": "text",
				"gen_ai.request.stream": false, "kaiak.usage.audio.input.duration": float64(c.seconds),
				"gen_ai.usage.input_tokens": float64(promptEstimate), "gen_ai.usage.output_tokens": 0.0,
				"kaiak.usage.cost_usd": 0.0, "kaiak.usage.estimated": true, "kaiak.usage.partial": false})
		})
	}

	// On asr, whose server reports no seconds: the request settles the length the
	// gateway measured from the upload, rounded up — a WAV of 2.5 s, an MP3 of 250
	// frames (6 s), a FLAC declaring 30.5 s — and the tokens the answer reports, the
	// prompt tokens read from the server's cache among them as cache reads, nothing
	// estimated.
	for _, c := range []struct {
		name, filename, contentType, file string
		seconds                           int64
	}{
		{"WAV", "memo.wav", "audio/wav", wavUpload(8000, 40000), 3},
		{"MP3", "memo.mp3", "audio/mpeg", mp3Upload(250), 6},
		{"FLAC", "memo.flac", "audio/flac", flacUpload(16000, 16000*61/2), 31},
	} {
		t.Run("llama-server/"+c.name, func(t *testing.T) {
			ls.SetReply(fakebackend.Reply{Usage: &fakebackend.Usage{PromptTokens: 375, CompletionTokens: 12, CachedTokens: 300}})
			defer ls.SetReply(fakebackend.Reply{})
			id := "stt-llama-server-" + c.name
			contentType, body := writeForm(t, textPart("model", "asr"), filePart("file", c.filename, c.contentType, c.file),
				textPart("prompt", prompt), textPart("max_tokens", "200"))
			before := len(ls.Requests())
			answer := transcriptionAnswer(t, g.postForm(t, "/v1/audio/transcriptions", key, id, contentType, body))
			if usage, _ := answer["usage"].(map[string]any); answer["type"] != "transcript.text.done" ||
				usage["input_tokens"] != 375.0 || usage["output_tokens"] != 12.0 {
				t.Errorf("answer %v: want llama-server's, its usage relayed", answer)
			}

			reqs := ls.Requests()
			if len(reqs) != before+1 || reqs[len(reqs)-1].Path != "/v1/audio/transcriptions" {
				t.Fatalf("backend got %d requests, want 1 to /v1/audio/transcriptions", len(reqs)-before)
			}
			wantRebuilt(t, reqs[len(reqs)-1], contentType, body, llamaASRModel)
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.backend.id": "ls", "kaiak.backend.type": "llama-server",
				"kaiak.deployment.model": llamaASRModel, "gen_ai.operation.name": "generate_content", "gen_ai.output.type": "text",
				"gen_ai.request.stream": false, "kaiak.usage.audio.input.duration": float64(c.seconds),
				"gen_ai.usage.input_tokens": 375.0, "gen_ai.usage.cache_read.input_tokens": 300.0,
				"gen_ai.usage.output_tokens": 12.0, "kaiak.usage.cost_usd": 0.0, "kaiak.usage.estimated": false,
				"kaiak.usage.partial": false})
		})
	}

	// A body that is no form — JSON, no content type — an upload over max_upload_bytes,
	// declared or not, an upload the gateway cannot measure sent to asr — whose server
	// reports no seconds — a transcription to a model whose list names chat alone, and
	// a chat to a model whose list names speech-to-text alone: each refused in OpenAI's
	// shape before any backend.
	t.Run("refused before any backend", func(t *testing.T) {
		before := received()
		upload := func(model, filename, file string) (string, []byte) {
			return writeForm(t, textPart("model", model), filePart("file", filename, "audio/wav", file))
		}
		t.Run("a JSON body", func(t *testing.T) {
			r := g.post(t, "/v1/audio/transcriptions", key, "", map[string]any{"model": "whisper", "file": "UklGRg=="})
			wantRefusal(t, r, http.StatusBadRequest, "unsupported_content_type")
		})
		t.Run("no content type", func(t *testing.T) {
			_, body := upload("whisper", "memo.wav", wavUpload(8000, 40000))
			wantRefusal(t, g.postForm(t, "/v1/audio/transcriptions", key, "", "", body), http.StatusBadRequest, "unsupported_content_type")
		})
		wantMessage := fmt.Sprintf("The request body exceeds the limit of %d bytes.", transcriptionUploadCap)
		t.Run("over the upload cap", func(t *testing.T) {
			contentType, body := upload("whisper", "long.wav", wavUpload(8000, transcriptionUploadCap))
			e := wantRefusal(t, g.postForm(t, "/v1/audio/transcriptions", key, "", contentType, body),
				http.StatusRequestEntityTooLarge, "request_too_large")
			if e["message"] != wantMessage {
				t.Errorf("message %q, want %q", e["message"], wantMessage)
			}
		})
		t.Run("over the upload cap, its length undeclared", func(t *testing.T) {
			contentType, body := upload("asr", "long.wav", wavUpload(8000, transcriptionUploadCap))
			e := wantRefusal(t, g.postUpload(t, "/v1/audio/transcriptions", key, "", contentType, io.MultiReader(strings.NewReader(string(body)))),
				http.StatusRequestEntityTooLarge, "request_too_large")
			if e["message"] != wantMessage {
				t.Errorf("message %q, want %q", e["message"], wantMessage)
			}
		})
		t.Run("an upload asr's server would not count", func(t *testing.T) {
			contentType, body := upload("asr", "pomegranate-interview.m4a", m4aUpload)
			e := wantRefusal(t, g.postForm(t, "/v1/audio/transcriptions", key, "", contentType, body),
				http.StatusBadRequest, "audio_format_unsupported")
			if e["param"] != "file" || strings.Contains(fmt.Sprint(e["message"]), "pomegranate") {
				t.Errorf("refusal %v, want param file and the filename never named", e)
			}
		})
		t.Run("a transcription to a model not listing it", func(t *testing.T) {
			contentType, body := upload("chat", "memo.wav", wavUpload(8000, 40000))
			e := wantRefusal(t, g.postForm(t, "/v1/audio/transcriptions", key, "", contentType, body), http.StatusBadRequest, "endpoint_not_served")
			if want := "The model is not served on /v1/audio/transcriptions: its configured endpoints do not include this API."; e["message"] != want {
				t.Errorf("message %q, want %q", e["message"], want)
			}
		})
		t.Run("a chat to a speech-to-text model", func(t *testing.T) {
			wantRefusal(t, g.post(t, "/v1/chat/completions", key, "", chatBody("asr", false, nil)), http.StatusBadRequest, "endpoint_not_served")
		})
		if n := received() - before; n != 0 {
			t.Errorf("the backends got %d requests for refused ones", n)
		}
	})

	// The group's 5 seconds of audio processed a minute, counted after each request —
	// nothing is reserved: 3 s on whisper, then 3 s on asr admitted with 2 s left,
	// taking the window to 6; the next transcription on either is refused, 429
	// audio_seconds, before its backend; chat in the group is not.
	t.Run("audio processed per minute", func(t *testing.T) {
		transcribe := func(model, id string) *response {
			contentType, body := writeForm(t, textPart("model", model), filePart("file", "memo.wav", "audio/wav", wavUpload(8000, 40000)))
			return g.postForm(t, "/v1/audio/transcriptions", listenKey, id, contentType, body)
		}
		vl.SetReply(fakebackend.Reply{AudioMillis: 2500})
		defer vl.SetReply(fakebackend.Reply{})
		for _, model := range []string{"whisper", "asr"} {
			id := "listen-" + model
			transcriptionAnswer(t, transcribe(model, id))
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.usage.audio.input.duration": 3.0})
		}
		before := received()
		for _, model := range []string{"whisper", "asr"} {
			id := "listen-refused-" + model
			r := transcribe(model, id)
			e, _ := r.json(t)["error"].(map[string]any)
			if r.StatusCode != http.StatusTooManyRequests || e["code"] != "rate_limit_exceeded" || e["type"] != "audio_seconds" ||
				r.Header.Get("Retry-After") == "" {
				t.Fatalf("%s: %d %.300s (Retry-After %q), want 429 audio_seconds with a wait", id, r.StatusCode, r.body, r.Header.Get("Retry-After"))
			}
			wantLine(t, g.settled(t, id), map[string]any{"kaiak.limit.scope": "group", "kaiak.limit.group": "listen",
				"kaiak.limit.type": "audio_seconds_in_per_minute", "kaiak.limit.configured": 5.0, "kaiak.limit.used": 6.0})
		}
		if n := received() - before; n != 0 {
			t.Errorf("the backends got %d refused transcriptions", n)
		}
		if got := g.metric(t, `kaiak_limit_rejections_total{kaiak_limit_scope="group",kaiak_limit_type="audio_seconds_in_per_minute"}`); got != 2 {
			t.Errorf("audio-processed limit rejections = %v, want 2", got)
		}
		if r := g.post(t, "/v1/chat/completions", listenKey, "", chatBody("chat", false, nil)); r.StatusCode != http.StatusOK {
			t.Errorf("chat in the group: %d %s, want 200", r.StatusCode, r.body)
		}
	})

	// The usage metrics count the seconds of audio processed per key and model, under
	// generate_content and text: whisper's 3 and 5, asr's 3, 6 and 31 for k; 3 each for
	// k-listen.
	usage := func(key, group, model string) string {
		return `{kaiak_key_group="` + group + `",kaiak_key_root_group="` + group + `",kaiak_key_id="` + key +
			`",gen_ai_request_model="` + model + `",gen_ai_operation_name="generate_content",gen_ai_output_type="text",kaiak_usage_status="complete"}`
	}
	for series, want := range map[string]float64{
		`kaiak_usage_audio_input_duration_seconds_total` + usage("k", "w", "whisper"):             8,
		`kaiak_usage_audio_input_duration_seconds_total` + usage("k", "w", "asr"):                 40,
		`kaiak_usage_audio_input_duration_seconds_total` + usage("k-listen", "listen", "whisper"): 3,
		`kaiak_usage_audio_input_duration_seconds_total` + usage("k-listen", "listen", "asr"):     3,
		`kaiak_usage_records_total` + usage("k", "w", "asr"):                                      3,
	} {
		if got := g.metric(t, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	// Nothing of the prompt, the filenames or the text reaches the log.
	for _, content := range []string{prompt, "tangerine-holiday", "pomegranate-interview", "memo.wav", transcriptionText} {
		if strings.Contains(g.logs.text(), content) {
			t.Errorf("the log holds %q", content)
		}
	}
	g.stop(t)
}

// A speech-to-text model on two vllm deployments, the first on a backend whose base_url
// leaves out the API version path (docs/specs/GATEWAY.md, Providers → A path the server
// does not have; Routing and reliability → Retries, Circuit breaker): a transcription
// routed there is answered 502 upstream_path_missing at once — one attempt, the
// backend's text not relayed, never retried on the second deployment, which answers its
// own share. The deployment's circuit, opening after 2 failures, stays closed however
// many such answers come — each counted as path_missing — so the deployment keeps its
// share of the requests.
func TestTranscriptionWrongBaseURL(t *testing.T) {
	lost, good := fakebackend.New(), fakebackend.New()
	t.Cleanup(lost.Close)
	t.Cleanup(good.Close)
	lost.SetNoRoute(vllmRouteMissing)
	good.SetModels(vllmASRModel)
	key, hash := newKey()
	configFile := filepath.Join(t.TempDir(), "config.json")
	writeJSON(t, configFile, map[string]any{
		"protocol_version": 8,
		"global":           map[string]any{"circuit": map[string]any{"failure_threshold": 2}},
		"backends": map[string]any{
			"lost": map[string]any{"type": "vllm", "base_url": lost.URL()},
			"vl":   map[string]any{"type": "vllm", "base_url": good.URL() + "/v1"},
		},
		"models": map[string]any{"whisper": map[string]any{
			"deployments": []any{
				map[string]any{"backend": "lost", "model": vllmASRModel},
				map[string]any{"backend": "vl", "model": vllmASRModel},
			},
			"endpoints": []any{"audio_transcriptions"},
			"metadata": map[string]any{"context_length": 8192,
				"capabilities": map[string]any{"streaming": false, "tools": false, "vision": false, "reasoning": false}},
		}},
		"groups": map[string]any{"w": map[string]any{"capture": false, "allowed_models": []any{"*"}}},
		"keys":   map[string]any{"k": map[string]any{"hash": hash, "group": "w"}},
	})
	g := startGateway(t, configFile)

	transcriptions := func(b *fakebackend.Backend, path string) int {
		n := 0
		for _, req := range b.Requests() {
			if req.Path == path {
				n++
			}
		}
		return n
	}
	refused, served := 0, 0
	for i := range 8 {
		id := fmt.Sprintf("lost-%d", i)
		contentType, body := writeForm(t, textPart("model", "whisper"), filePart("file", "memo.wav", "audio/wav", wavUpload(8000, 40000)))
		before := len(good.Requests())
		r := g.postForm(t, "/v1/audio/transcriptions", key, id, contentType, body)
		line := g.settled(t, id)
		switch {
		case r.StatusCode == http.StatusBadGateway && openAIErrorCode(t, r) == "upstream_path_missing" &&
			!strings.Contains(string(r.body), "Not Found"):
			refused++
			if n := len(good.Requests()) - before; n != 0 {
				t.Errorf("%s: retried on the second deployment (%d requests)", id, n)
			}
			wantLine(t, line, map[string]any{"kaiak.backend.id": "lost", "kaiak.attempts": 1.0, "error.type": "upstream_path_missing"})
		case r.StatusCode == http.StatusOK:
			served++
			transcriptionAnswer(t, r)
		default:
			t.Fatalf("%s: %d %s, want 200 from vl or 502 upstream_path_missing from lost", id, r.StatusCode, r.body)
		}
	}
	if got := transcriptions(lost, "/audio/transcriptions"); refused <= 2 || got != refused {
		t.Errorf("lost got %d transcriptions and %d were answered upstream_path_missing, want them equal and above the threshold of 2",
			got, refused)
	}
	if got := transcriptions(good, "/v1/audio/transcriptions"); got != served {
		t.Errorf("vl got %d transcriptions, want the %d served", got, served)
	}
	deployment := `kaiak_backend_id="lost",kaiak_deployment_model="` + vllmASRModel + `"`
	if v := g.metric(t, `kaiak_circuit_state{`+deployment+`,kaiak_circuit_state="open"}`); v != 0 {
		t.Errorf("lost's circuit open = %v, want closed", v)
	}
	if v := g.metric(t, `kaiak_upstream_attempts_total{`+deployment+`,kaiak_attempt_outcome="path_missing"}`); v != float64(refused) {
		t.Errorf("path_missing attempts = %v, want %d", v, refused)
	}
	g.stop(t)
}
