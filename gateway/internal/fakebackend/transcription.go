package fakebackend

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Speech-to-text at /v1/audio/transcriptions, in vLLM's shape or llama-server's
// (SetTranscriptionShape): an upload, its body read as a multipart/form-data form
// (readUploadForm); one sent without its Content-Length is refused 400, as on image
// edits, and one without a file part 400, as both servers refuse it. Stream true is
// refused 400: both servers stream a transcription, and the gateway never asks for
// one. The text is the reply's chunks joined, the audio heard AudioMillis long, the
// tokens the reply's usage; OmitUsage leaves usage out. The reply's stream fault stops
// the answer halfway: Cut drops the connection, Hang waits for the request's end, End
// ends the body there.

// transcriptionPath is the speech-to-text endpoint, below the version prefix.
const transcriptionPath = "audio/transcriptions"

// TranscriptionShape is the server whose speech-to-text the backend plays.
type TranscriptionShape int

const (
	// VLLMTranscription is vLLM's (vllm/entrypoints/speech_to_text/, 0.30.0;
	// docs/plans/media-phase-b/RESEARCH.md): its model check — a model part naming a
	// model the models list does not (SetModels) is answered 404 "The model `X` does
	// not exist." — then the answer by response_format: json (absent), text — JSON
	// too — and diarized_json, {"text", "usage": {"type": "duration", "seconds"}}, the
	// seconds the audio's length rounded up; verbose_json, {"duration", "language",
	// "text", "segments", "words"}, the exact length and no usage; srt, vtt and any
	// other format refused 400. A model without transcription (SetNonAudioModels) has
	// no route: 404 {"detail":"Not Found"}.
	VLLMTranscription TranscriptionShape = iota
	// LlamaServerTranscription is llama-server's (tools/server/server-chat.cpp,
	// b11526): the model part is not checked — a server serves its one model —
	// response_format json only (absent means it), else 400; the answer
	// {"type": "transcript.text.done", "text", "usage": {"type": "tokens",
	// "input_tokens", "output_tokens", "total_tokens", "input_tokens_details"}}, no
	// seconds. A model without an audio encoder (SetNonAudioModels) is answered 501
	// not_supported_error "The current model does not support audio input.".
	LlamaServerTranscription
)

// SetTranscriptionShape scripts which server's speech-to-text the backend plays from
// now on (the zero value: vLLM's).
func (b *Backend) SetTranscriptionShape(shape TranscriptionShape) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transcriptionShape = shape
}

// SetNonAudioModels scripts, from now on, the models whose transcription the backend
// answers as its server answers one on a model that does not hear audio: vLLM mounts no
// transcription route (404 {"detail":"Not Found"}), llama-server answers 501
// not_supported_error (nil: every model hears).
func (b *Backend) SetNonAudioModels(models ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nonAudio = models
}

// llamaServerError is llama-server's error answer (tools/server/server-common.cpp,
// format_error_response): its code the status.
func llamaServerError(status int, errType, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": status, "message": message, "type": errType}}
}

// writeTranscription answers a transcription in shape, its body req.Body as received;
// models are the models list's (nil: DefaultModels).
func (b *Backend) writeTranscription(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	shape TranscriptionShape, models, nonAudio []string) {
	refuse := func(status int, message string) {
		if shape == LlamaServerTranscription {
			writeJSON(w, status, llamaServerError(status, "invalid_request_error", message))
			return
		}
		writeJSON(w, status, vllmError(status, "BadRequestError", message))
	}
	form, ok := readUploadForm(r.Header.Get("Content-Type"), req.Body)
	model, _ := form.value("model")
	if shape == VLLMTranscription && slices.Contains(nonAudio, model) {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Not Found"})
		return
	}
	switch {
	case r.ContentLength < 0:
		refuse(http.StatusBadRequest, "the fake takes an upload with its Content-Length")
		return
	case !ok:
		refuse(http.StatusBadRequest, "the form does not parse")
		return
	case form.files["file"] != 1:
		refuse(http.StatusBadRequest, "file: Field required")
		return
	}
	if models == nil {
		models = DefaultModels
	}
	if shape == VLLMTranscription && model != "" && !slices.Contains(models, model) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": "The model `" + model + "` does not exist.", "type": "NotFoundError", "param": "model", "code": http.StatusNotFound}})
		return
	}
	if shape == LlamaServerTranscription && slices.Contains(nonAudio, model) {
		writeJSON(w, http.StatusNotImplemented, llamaServerError(http.StatusNotImplemented, "not_supported_error",
			"The current model does not support audio input."))
		return
	}
	if stream, _ := form.value("stream"); strings.EqualFold(stream, "true") {
		refuse(http.StatusBadRequest, "the fake does not stream transcriptions")
		return
	}
	format, ok := form.value("response_format")
	if !ok {
		format = "json"
	}
	chunks := reply.Chunks
	if chunks == nil {
		chunks = DefaultChunks
	}
	text := strings.Join(chunks, "")
	usage := Usage{PromptTokens: 7, CompletionTokens: len(chunks)}
	if reply.Usage != nil {
		usage = *reply.Usage
	}
	millis := reply.AudioMillis
	if millis == 0 {
		millis = DefaultAudioMillis
	}
	var answer any
	switch {
	case shape == LlamaServerTranscription && format == "json":
		body := map[string]any{"type": "transcript.text.done", "text": text}
		if !reply.OmitUsage {
			body["usage"] = map[string]any{"type": "tokens", "input_tokens": usage.PromptTokens,
				"output_tokens": usage.CompletionTokens, "total_tokens": usage.PromptTokens + usage.CompletionTokens,
				"input_tokens_details": map[string]int{"text_tokens": 0, "audio_tokens": usage.PromptTokens}}
		}
		answer = body
	case shape == VLLMTranscription && (format == "json" || format == "text" || format == "diarized_json"):
		body := map[string]any{"text": text}
		if !reply.OmitUsage {
			body["usage"] = map[string]any{"type": "duration", "seconds": int(math.Ceil(float64(millis) / 1000))}
		}
		answer = body
	case shape == VLLMTranscription && format == "verbose_json":
		duration := float64(millis) / 1000
		answer = struct {
			Duration float64 `json:"duration"`
			Language string  `json:"language"`
			Text     string  `json:"text"`
			Segments []any   `json:"segments"`
			Words    []any   `json:"words"`
		}{duration, "en", text, []any{map[string]any{"id": 0, "seek": 0, "start": 0, "end": duration, "text": text}}, nil}
	default:
		refuse(http.StatusBadRequest, fmt.Sprintf("the fake does not answer response_format %q", format))
		return
	}
	data, _ := json.Marshal(answer) // test values always encode
	b.writeBodyAnswer(w, r, req, reply, data)
}

// writeBodyAnswer writes a JSON answer, data, with its length — or, with the reply's
// stream fault, its first half, then the fault.
func (b *Backend) writeBodyAnswer(w http.ResponseWriter, r *http.Request, req *Request, reply Reply, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	fault := reply.Fault
	if fault == nil {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data[:len(data)/2])
	_ = http.NewResponseController(w).Flush()
	switch fault.Kind {
	case Cut:
		panic(http.ErrAbortHandler)
	case Hang:
		b.waitForCancel(r, req)
	case End:
	default:
		panic(fmt.Sprintf("fakebackend: a body answer's fault of Kind %d", fault.Kind))
	}
}
