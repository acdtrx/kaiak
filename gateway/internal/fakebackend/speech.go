package fakebackend

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// Speech, as vLLM-Omni serves it (vllm_omni/entrypoints/openai/serving_speech.py,
// v0.30.0), at /v1/audio/speech: the whole clip — a WAV file with its real sizes, or
// PCM — with its token usage in X-VLLM-OMNI-* response headers; for stream_format
// "audio", the raw-audio stream — the audio's bytes as they are generated, chunked, a
// WAV stream opening with a 44-byte header whose two size fields are the placeholder
// 0xFFFFFFFF, no usage; for stream true or stream_format "sse", the SSE stream — a
// speech.audio.delta event per piece of audio, base64, a WAV stream's header a delta
// of its own, then speech.audio.done carrying the usage. The audio is 16-bit mono
// silence at the request's sample_rate (DefaultSampleRate when it names none),
// AudioMillis long, in len(Chunks) pieces on either stream. A format other than wav
// and pcm is refused 400, as is a stream_format other than sse and audio, as vLLM-Omni
// refuses it.

// speechPath is the speech endpoint, below the version prefix.
const speechPath = "audio/speech"

// DefaultAudioMillis is a speech answer's length when its Reply sets no AudioMillis.
const DefaultAudioMillis = 2500

// DefaultSampleRate is the rate of a speech answer whose request names no
// sample_rate: Qwen3-TTS's native 24 kHz.
const DefaultSampleRate = 24000

// speechFrameBytes: a sample frame of 16-bit mono.
const speechFrameBytes = 2

// SetNonSpeechModels scripts, from now on, the models whose speech the backend answers
// as vLLM-Omni answers speech on a model with no speech stage: 400, "TTS model did not
// produce audio output." — and a speech batch as on a pure-diffusion server: 400,
// "Batch speech is not supported in diffusion mode" (nil: every model speaks).
func (b *Backend) SetNonSpeechModels(models ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nonSpeech = models
}

// omniError is vLLM-Omni's error answer: OpenAI's shape, its code the status.
func omniError(status int, errType, message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "param": nil, "code": status}}
}

// writeSpeech answers a speech request for model, its audio in len(chunks) pieces on
// the raw-audio stream, its usage the output tokens' count.
func (b *Backend) writeSpeech(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	top map[string]json.RawMessage, model string, chunks []string, usage Usage, nonSpeech []string) {
	if slices.Contains(nonSpeech, model) {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			"TTS model did not produce audio output."))
		return
	}
	var format, streamFormat string
	var stream bool
	_ = json.Unmarshal(top["response_format"], &format)
	_ = json.Unmarshal(top["stream_format"], &streamFormat)
	_ = json.Unmarshal(top["stream"], &stream)
	rate := DefaultSampleRate
	if raw, ok := top["sample_rate"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &rate) != nil || rate <= 0 {
			writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError", "sample_rate must be above 0"))
			return
		}
	}
	if format == "" {
		format = "wav"
	}
	raw := streamFormat == "audio"
	switch {
	case format != "wav" && format != "pcm":
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			fmt.Sprintf("the fake does not play response_format %q", format)))
		return
	case streamFormat != "" && streamFormat != "audio" && streamFormat != "sse":
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			"stream_format must be 'sse' or 'audio'"))
		return
	}

	millis := reply.AudioMillis
	if millis == 0 {
		millis = DefaultAudioMillis
	}
	frames := rate * millis / 1000
	audio := make([]byte, frames*speechFrameBytes)
	if !raw && (stream || streamFormat == "sse") {
		b.writeSpeechEvents(w, r, req, reply, format, rate, audio, max(len(chunks), 1), usage)
		return
	}
	w.Header().Set("Content-Type", "audio/"+format)
	if raw {
		b.writeAudioStream(w, r, req, reply, format, rate, audio, max(len(chunks), 1))
		return
	}
	if format == "wav" {
		audio = append(WAVHeader(rate, len(audio), false), audio...)
	}
	if !reply.OmitUsage {
		// vLLM-Omni writes them upper-case; header names are case-insensitive.
		h := w.Header()
		h["X-VLLM-OMNI-INPUT-TOKENS"] = []string{strconv.Itoa(usage.PromptTokens)}
		h["X-VLLM-OMNI-OUTPUT-TOKENS"] = []string{strconv.Itoa(usage.CompletionTokens)}
		h["X-VLLM-OMNI-TOTAL-TOKENS"] = []string{strconv.Itoa(usage.PromptTokens + usage.CompletionTokens)}
		h["X-VLLM-OMNI-INPUT-TEXT-TOKENS"] = []string{strconv.Itoa(usage.PromptTokens)}
		h["X-VLLM-OMNI-INPUT-AUDIO-TOKENS"] = []string{"0"}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// writeAudioStream writes the raw-audio stream: the audio in pieces of whole frames,
// one per paced event — a WAV stream's header written with the first, as vLLM-Omni's
// _generate_audio_chunks yields it right before the first audio chunk — the reply's
// Fault before piece At (Hang, Cut, End): a fault at 0 comes before any byte.
func (b *Backend) writeAudioStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	format string, rate int, audio []byte, pieces int) {
	w.WriteHeader(http.StatusOK)
	s := &streamWriter{b: b, w: w, r: r, req: req, reply: reply, flusher: http.NewResponseController(w)}
	frames := len(audio) / speechFrameBytes
	for i := range pieces {
		if !s.interrupt(i) || !s.await() {
			return
		}
		from, to := frames*i/pieces*speechFrameBytes, frames*(i+1)/pieces*speechFrameBytes
		piece := audio[from:to]
		if i == 0 && format == "wav" {
			piece = append(WAVHeader(rate, 0, true), piece...)
		}
		if _, err := w.Write(piece); err != nil || s.flusher.Flush() != nil {
			return
		}
	}
}

// writeSpeechEvents writes the SSE stream as vLLM-Omni's _generate_audio_sse_events
// does: a speech.audio.delta per piece of audio — {"type", "audio": base64,
// "response_format"} — a WAV stream's header first, a delta of its own; then
// speech.audio.done with the usage (none with OmitUsage). The reply's stream fault
// applies before piece At — At 0 before the header too — its error event
// speech.audio.error: server_error (or the fault's Code as its type), code 500, and
// partial_audio and action "discard" once a delta was sent.
func (b *Backend) writeSpeechEvents(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	format string, rate int, audio []byte, pieces int, usage Usage) {
	var s *streamWriter
	errorEvent := func(errType string) []byte {
		if errType == "" {
			errType = "server_error"
		}
		detail := map[string]any{"message": "CUDA error on device 3: the engine died serving Qwen/Qwen3-TTS-backend",
			"type": errType, "param": nil, "code": http.StatusInternalServerError}
		if s.sent > 0 {
			detail["partial_audio"], detail["action"] = true, "discard"
		}
		payload, _ := json.Marshal(map[string]any{"type": "speech.audio.error", "error": detail}) // test values always encode
		return payload
	}
	s, ok := b.startStream(w, r, req, reply, errorEvent)
	if !ok {
		return
	}
	event := func(v map[string]any) bool {
		payload, _ := json.Marshal(v) // test values always encode
		return s.send(v["type"].(string), payload)
	}
	delta := func(p []byte) bool {
		return event(map[string]any{"type": "speech.audio.delta", "audio": base64.StdEncoding.EncodeToString(p),
			"response_format": format})
	}
	if !s.interrupt(0) {
		return
	}
	if format == "wav" && !delta(WAVHeader(rate, 0, true)) {
		return
	}
	frames := len(audio) / speechFrameBytes
	for i := range pieces {
		if !s.interrupt(i) {
			return
		}
		from, to := frames*i/pieces*speechFrameBytes, frames*(i+1)/pieces*speechFrameBytes
		if !delta(audio[from:to]) {
			return
		}
	}
	done := map[string]any{"type": "speech.audio.done"}
	if !reply.OmitUsage {
		done["usage"] = map[string]any{
			"input_tokens":        usage.PromptTokens,
			"output_tokens":       usage.CompletionTokens,
			"total_tokens":        usage.PromptTokens + usage.CompletionTokens,
			"input_token_details": map[string]any{"text_tokens": usage.PromptTokens, "audio_tokens": 0},
		}
	}
	event(done)
}

// WAVHeader is the 44-byte header of a WAV file of 16-bit mono PCM at rate whose data
// is dataBytes long — or, streamed, the placeholder 0xFFFFFFFF in both size fields, as
// vLLM-Omni's raw-audio stream writes. Every WAV answer of the fake opens with it, its
// data silence: a test builds the audio it expects relayed from it.
func WAVHeader(rate, dataBytes int, streamed bool) []byte {
	riffSize, dataSize := uint32(36+dataBytes), uint32(dataBytes)
	if streamed {
		riffSize, dataSize = 0xFFFFFFFF, 0xFFFFFFFF
	}
	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, riffSize)
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, 1) // mono
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*speechFrameBytes))
	h = binary.LittleEndian.AppendUint16(h, speechFrameBytes)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	return binary.LittleEndian.AppendUint32(h, dataSize)
}
