package main

// The speech-to-text checks, with -asr-base-url: a server of the run's kind serving a
// speech recognition model, sent the tone uploads.go writes, as WAV, FLAC and MP3, of
// a length known here. vLLM reports the seconds it decoded, which the record carries;
// llama-server reports tokens only, and the gateway measures the upload itself
// (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text). Either way the
// record's seconds must be the tone's length rounded up.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
)

// Request IDs of the speech-to-text checks' requests whose log lines they read; each
// format's transcription is idTranscribe followed by the format.
const (
	idTranscribe        = "live-transcribe-"
	idTranscribeVerbose = "live-transcribe-verbose"
	idTranscribeStream  = "live-transcribe-stream"
	idTranscribeOgg     = "live-transcribe-ogg"
	idTranscribeLimit   = "live-transcribe-limit"
	idTranscribeRefused = "live-transcribe-limit-refused"
)

// transcriptionPrompt is the prompt part every transcription carries: the gateway
// estimates a transcription's input tokens from it where the server reports none.
const transcriptionPrompt = "Transcribe the audio."

// oggUpload is an upload llama-server cannot decode, nor the gateway measure: an Ogg
// page's magic and nothing playable.
var oggUpload = uploadAudio{"ogg", "tone.ogg", "audio/ogg", append([]byte("OggS"), make([]byte, 60)...)}

// transcriptionForm is a transcription request to model of audio, with params added
// as text parts.
func transcriptionForm(model string, audio uploadAudio, params map[string]any) []formPart {
	parts := []formPart{textPart("model", model), filePart("file", audio.filename, audio.contentType, audio.data),
		textPart("prompt", transcriptionPrompt)}
	return withTextParts(parts, params)
}

// transcriptionAnswer is the part of a transcription the checks read, in either
// server's shape: vLLM's usage of type duration, llama-server's of type tokens;
// verbose_json's duration.
type transcriptionAnswer struct {
	Text     *string
	Duration *float64
	Usage    *struct {
		Type         string
		Seconds      *float64
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
}

func (r *run) transcriptionChecks() {
	for _, audio := range uploadAudios() {
		if want, ok := r.checkTranscription(audio); ok {
			r.checkMediaLog("usage-log/transcribe-"+audio.format, idTranscribe+audio.format, want)
		}
	}
	if r.o.kind == kindVLLM {
		r.checkTranscriptionVerbose()
	}
	r.checkTranscriptionRefused()
}

// checkTranscription transcribes audio: a text, and the usage its server reports — on
// vLLM the seconds it decoded, which must be the tone's length rounded up; on
// llama-server tokens. It returns what the request's log line must carry: those
// seconds, and the tokens reported, or estimated where none are.
func (r *run) checkTranscription(audio uploadAudio) (mediaLog, bool) {
	name, id := "transcribe-"+audio.format, idTranscribe+audio.format
	resp, err := r.postForm(r.key, "/v1/audio/transcriptions", id, transcriptionForm(modelASR, audio, nil))
	if !r.ok(name, resp, err, id) {
		return mediaLog{}, false
	}
	var a transcriptionAnswer
	if err := json.Unmarshal(resp.body, &a); err != nil || a.Text == nil {
		r.fail(name, "no text: %s", clip(string(resp.body)))
		return mediaLog{}, false
	}
	upload := fmt.Sprintf("%s of %d bytes, %.1f s", audio.format, len(audio.data), float64(uploadMillis)/1000)
	want := mediaLog{outputType: "text", secondsIn: uploadSeconds()}
	if r.o.kind == kindVLLM {
		switch {
		case a.Usage == nil || a.Usage.Type != "duration" || a.Usage.Seconds == nil:
			r.fail(name, "no usage of type duration with its seconds: %s", clip(string(resp.body)))
			return mediaLog{}, false
		case *a.Usage.Seconds != float64(uploadSeconds()):
			r.fail(name, "%s: the server counted %v s, want %d — the length rounded up", upload, *a.Usage.Seconds, uploadSeconds())
			return mediaLog{}, false
		}
		// vLLM reports no tokens for a whole answer: the gateway estimates them.
		want.estimated = true
		r.pass(name, fmt.Sprintf("%s: %q; the server counted %v s", upload, clip(*a.Text), *a.Usage.Seconds))
		return want, true
	}
	if a.Usage == nil || a.Usage.Type != "tokens" || a.Usage.InputTokens == nil || a.Usage.OutputTokens == nil {
		r.fail(name, "no usage of type tokens with its counts: %s", clip(string(resp.body)))
		return mediaLog{}, false
	}
	want.tokens = &speechUsage{InputTokens: *a.Usage.InputTokens, OutputTokens: *a.Usage.OutputTokens}
	r.pass(name, fmt.Sprintf("%s: %q; %d+%d tokens reported, no seconds", upload, clip(*a.Text), want.tokens.InputTokens,
		want.tokens.OutputTokens))
	return want, true
}

// checkTranscriptionVerbose (vLLM): verbose_json answers the decoded length as
// duration and no usage; the record's seconds are that duration rounded up. Only
// models with segment timestamps answer it — Whisper, FunASR, FireRedASR2 — and the
// others' 400, relayed from the server, skips the check.
func (r *run) checkTranscriptionVerbose() {
	const name, logName = "transcribe-verbose", "usage-log/transcribe-verbose"
	wav := uploadAudios()[0]
	resp, err := r.postForm(r.key, "/v1/audio/transcriptions", idTranscribeVerbose,
		transcriptionForm(modelASR, wav, map[string]any{"response_format": "verbose_json"}))
	if err == nil && resp.status == http.StatusBadRequest {
		if notSent, logErr := r.notSent(idTranscribeVerbose); logErr == nil && !notSent {
			r.skip(name, "the server refused verbose_json (400): only models with segment timestamps answer it — Whisper, FunASR, FireRedASR2")
			r.skip(logName, "no verbose_json answer")
			return
		}
	}
	if !r.ok(name, resp, err, idTranscribeVerbose) {
		return
	}
	var a transcriptionAnswer
	switch err := json.Unmarshal(resp.body, &a); {
	case err != nil || a.Duration == nil || a.Text == nil:
		r.fail(name, "no duration or text: %s", clip(string(resp.body)))
		return
	case int64(math.Ceil(*a.Duration)) != uploadSeconds():
		r.fail(name, "duration %v s, want %.1f — %d rounded up", *a.Duration, float64(uploadMillis)/1000, uploadSeconds())
		return
	}
	r.pass(name, fmt.Sprintf("duration %v s, %q", *a.Duration, clip(*a.Text)))
	r.checkMediaLog(logName, idTranscribeVerbose, mediaLog{outputType: "text", estimated: true, secondsIn: uploadSeconds()})
}

// checkTranscriptionRefused: a stream is refused before the backend, on both servers;
// on llama-server, so is an upload the gateway cannot measure — llama-server decodes
// WAV, FLAC and MP3 alone, and its answer carries no seconds.
func (r *run) checkTranscriptionRefused() {
	transcriptions := "/v1/audio/transcriptions"
	refusals := []refusalCase{{what: "stream=true", path: transcriptions, id: idTranscribeStream,
		body:   transcriptionForm(modelASR, uploadAudios()[0], map[string]any{"stream": "true"}),
		status: http.StatusBadRequest, code: "invalid_value", param: "stream"}}
	if r.o.kind == kindLlamaServer {
		refusals = append(refusals, refusalCase{what: "an Ogg upload", path: transcriptions, id: idTranscribeOgg,
			body: transcriptionForm(modelASR, oggUpload, nil), status: http.StatusBadRequest, code: "audio_format_unsupported",
			param: "file"})
	}
	r.checkRefusals("transcribe-refused", refusals)
}

// checkTranscriptionLimit: audio processed settles after the request, so a first
// transcription fills a 1-second window and the next is refused.
func (r *run) checkTranscriptionLimit() {
	wav := uploadAudios()[0]
	r.checkMediaLimit("transcribe-limit", func(id string) (*response, error) {
		return r.postForm(r.mediaKey, "/v1/audio/transcriptions", id, transcriptionForm(modelASR, wav, nil))
	}, idTranscribeLimit, idTranscribeRefused, "audio_seconds", "audio_seconds_in_per_minute")
}
