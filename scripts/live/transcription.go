package main

// The speech-to-text checks, with -asr-base-url: a server of the run's kind serving a
// speech recognition model, sent the audio uploads.go writes — a tone as WAV and FLAC,
// silence as MP3 — of a length known here. vLLM reports the seconds it decoded, which
// the record must carry, whatever the upload measures; llama-server reports tokens
// only, and the gateway measures the upload itself, so the record carries the upload's
// length rounded up (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strings"
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
// server's shape: vLLM's usage of type duration, llama-server's of type tokens — its
// input including the prompt tokens read from the server's cache, reported in
// input_tokens_details; verbose_json's duration.
type transcriptionAnswer struct {
	Text     *string
	Duration *float64
	Usage    *struct {
		Type               string
		Seconds            *float64
		InputTokens        *int64 `json:"input_tokens"`
		OutputTokens       *int64 `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
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

// heardSeconds is what a vLLM speech-to-text server must report hearing in each
// upload, rounded up, and what that length is.
func (r *run) heardSeconds() (int64, string) {
	if r.o.asrHeardMillis == 0 {
		return uploadSeconds(), fmt.Sprintf("the upload's %.1f s", float64(uploadMillis)/1000)
	}
	return ceilRat(big.NewRat(int64(r.o.asrHeardMillis), 1000)),
		fmt.Sprintf("the %.1f s the fake hears", float64(r.o.asrHeardMillis)/1000)
}

// checkTranscription transcribes audio: a text, and the usage its server reports — on
// vLLM the seconds it decoded (heardSeconds); on llama-server tokens. It returns what
// the request's log line must carry: vLLM's seconds, or on llama-server the upload's
// length rounded up; and the tokens reported, with their cache reads, or estimated
// where none are.
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
	if r.o.kind == kindVLLM {
		heard, heardWhat := r.heardSeconds()
		switch {
		case a.Usage == nil || a.Usage.Type != "duration" || a.Usage.Seconds == nil:
			r.fail(name, "no usage of type duration with its seconds: %s", clip(string(resp.body)))
			return mediaLog{}, false
		case *a.Usage.Seconds != float64(heard):
			r.fail(name, "%s: the server counted %v s, want %d — %s rounded up", upload, *a.Usage.Seconds, heard, heardWhat)
			return mediaLog{}, false
		}
		r.pass(name, fmt.Sprintf("%s: %q; the server counted %v s", upload, clip(*a.Text), *a.Usage.Seconds))
		// vLLM reports no tokens for a whole answer: the gateway estimates them.
		return mediaLog{outputType: "text", estimated: true, secondsIn: int64(math.Ceil(*a.Usage.Seconds)),
			secondsInFrom: "the seconds the server reported"}, true
	}
	want := mediaLog{outputType: "text", secondsIn: uploadSeconds(),
		secondsInFrom: "the upload's length rounded up, measured by the gateway"}
	if a.Usage == nil || a.Usage.Type != "tokens" || a.Usage.InputTokens == nil || a.Usage.OutputTokens == nil {
		r.fail(name, "no usage of type tokens with its counts: %s", clip(string(resp.body)))
		return mediaLog{}, false
	}
	want.tokens = &speechUsage{InputTokens: *a.Usage.InputTokens, OutputTokens: *a.Usage.OutputTokens}
	if d := a.Usage.InputTokensDetails; d != nil {
		want.cacheRead = d.CachedTokens
	}
	r.pass(name, fmt.Sprintf("%s: %q; %d+%d tokens reported, %d read from the cache, no seconds", upload, clip(*a.Text),
		want.tokens.InputTokens, want.tokens.OutputTokens, want.cacheRead))
	return want, true
}

// verboseUnsupported is how vLLM begins its 400 to verbose_json on a model without
// segment timestamps (vllm/entrypoints/speech_to_text/base/serving.py, 0.30.0):
// "Currently do not support verbose_json for <model>".
const verboseUnsupported = "Currently do not support verbose_json"

// checkTranscriptionVerbose (vLLM): verbose_json answers the decoded length as
// duration and no usage; the record's seconds are that duration rounded up. Only
// models with segment timestamps answer it — Whisper, FunASR, FireRedASR2 — and the
// others' refusal (verboseUnsupported), relayed from the server, skips the check; any
// other answer but a 200 fails it.
func (r *run) checkTranscriptionVerbose() {
	const name, logName = "transcribe-verbose", "usage-log/transcribe-verbose"
	wav := uploadAudios()[0]
	resp, err := r.postForm(r.key, "/v1/audio/transcriptions", idTranscribeVerbose,
		transcriptionForm(modelASR, wav, map[string]any{"response_format": "verbose_json"}))
	if err == nil && resp.status == http.StatusBadRequest {
		var refusal struct{ Error struct{ Message string } }
		// A body that does not parse leaves the message empty: the check fails below.
		_ = json.Unmarshal(resp.body, &refusal)
		notSent, logErr := r.notSent(idTranscribeVerbose)
		if strings.HasPrefix(refusal.Error.Message, verboseUnsupported) && logErr == nil && !notSent {
			r.skip(name, fmt.Sprintf("the server refused verbose_json (%q): only models with segment timestamps "+
				"answer it — Whisper, FunASR, FireRedASR2", refusal.Error.Message))
			r.skip(logName, "no verbose_json answer")
			return
		}
	}
	if !r.ok(name, resp, err, idTranscribeVerbose) {
		return
	}
	heard, heardWhat := r.heardSeconds()
	var a transcriptionAnswer
	switch err := json.Unmarshal(resp.body, &a); {
	case err != nil || a.Duration == nil || a.Text == nil:
		r.fail(name, "no duration or text: %s", clip(string(resp.body)))
		return
	case int64(math.Ceil(*a.Duration)) != heard:
		r.fail(name, "duration %v s, want %s — %d rounded up", *a.Duration, heardWhat, heard)
		return
	}
	r.pass(name, fmt.Sprintf("duration %v s, %q", *a.Duration, clip(*a.Text)))
	r.checkMediaLog(logName, idTranscribeVerbose, mediaLog{outputType: "text", estimated: true,
		secondsIn: int64(math.Ceil(*a.Duration)), secondsInFrom: "the server's duration rounded up"})
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
