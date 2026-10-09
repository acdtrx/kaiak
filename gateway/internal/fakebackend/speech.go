package fakebackend

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
)

// Speech, as vLLM-Omni serves it (vllm_omni/entrypoints/openai/serving_speech.py,
// v0.30.0), at /v1/audio/speech: the whole clip — a WAV file with its real sizes, or
// PCM — with its token usage in X-VLLM-OMNI-* response headers; or, for
// stream_format "audio", the raw-audio stream — the audio's bytes as they are
// generated, chunked, a WAV stream opening with a 44-byte header whose two size
// fields are the placeholder 0xFFFFFFFF, no usage. The audio is 16-bit mono silence at
// the request's sample_rate (DefaultSampleRate when it names none), AudioMillis long.
// The SSE mode is not played: a request asking for it is refused 400, as is a format
// other than wav and pcm; a stream_format other than sse and audio is refused 400, as
// vLLM-Omni refuses it.

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
// produce audio output." (nil: every model speaks).
func (b *Backend) SetNonSpeechModels(models ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nonSpeech = models
}

// speechError is vLLM-Omni's error answer: OpenAI's shape, its code the status.
func speechError(status int, errType, message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "param": nil, "code": status}}
}

// writeSpeech answers a speech request for model, its audio in len(chunks) pieces on
// the raw-audio stream, its usage the output tokens' count.
func (b *Backend) writeSpeech(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	top map[string]json.RawMessage, model string, chunks []string, usage Usage, nonSpeech []string) {
	if slices.Contains(nonSpeech, model) {
		writeJSON(w, http.StatusBadRequest, speechError(http.StatusBadRequest, "BadRequestError",
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
			writeJSON(w, http.StatusBadRequest, speechError(http.StatusBadRequest, "BadRequestError", "sample_rate must be above 0"))
			return
		}
	}
	if format == "" {
		format = "wav"
	}
	raw := streamFormat == "audio"
	switch {
	case format != "wav" && format != "pcm":
		writeJSON(w, http.StatusBadRequest, speechError(http.StatusBadRequest, "BadRequestError",
			fmt.Sprintf("the fake does not play response_format %q", format)))
		return
	case streamFormat != "" && streamFormat != "audio" && streamFormat != "sse":
		writeJSON(w, http.StatusBadRequest, speechError(http.StatusBadRequest, "BadRequestError",
			"stream_format must be 'sse' or 'audio'"))
		return
	case !raw && (stream || streamFormat == "sse"):
		writeJSON(w, http.StatusBadRequest, speechError(http.StatusBadRequest, "BadRequestError",
			"the fake does not play the SSE mode"))
		return
	}

	millis := reply.AudioMillis
	if millis == 0 {
		millis = DefaultAudioMillis
	}
	frames := rate * millis / 1000
	audio := make([]byte, frames*speechFrameBytes)
	w.Header().Set("Content-Type", "audio/"+format)
	if raw {
		b.writeAudioStream(w, r, req, reply, format, rate, audio, max(len(chunks), 1))
		return
	}
	if format == "wav" {
		audio = append(wavHeader(rate, len(audio), false), audio...)
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

// writeAudioStream writes the raw-audio stream: a WAV stream's header first, flushed
// on its own, then the audio in pieces of whole frames, one per paced event — the
// reply's Fault before piece At (Hang, Cut, End).
func (b *Backend) writeAudioStream(w http.ResponseWriter, r *http.Request, req *Request, reply Reply,
	format string, rate int, audio []byte, pieces int) {
	w.WriteHeader(http.StatusOK)
	s := &streamWriter{b: b, w: w, r: r, req: req, reply: reply, flusher: http.NewResponseController(w)}
	if format == "wav" {
		if _, err := w.Write(wavHeader(rate, 0, true)); err != nil || s.flusher.Flush() != nil {
			return
		}
	}
	frames := len(audio) / speechFrameBytes
	for i := range pieces {
		if !s.interrupt(i) || !s.await() {
			return
		}
		from, to := frames*i/pieces*speechFrameBytes, frames*(i+1)/pieces*speechFrameBytes
		if _, err := w.Write(audio[from:to]); err != nil || s.flusher.Flush() != nil {
			return
		}
	}
}

// wavHeader is the 44-byte header of a WAV file of 16-bit mono PCM at rate whose data
// is dataBytes long — or, streamed, the placeholder 0xFFFFFFFF in both size fields, as
// vLLM-Omni's raw-audio stream writes.
func wavHeader(rate, dataBytes int, streamed bool) []byte {
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
