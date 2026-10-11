package server

import (
	"context"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// transcriptionUpload is speech-to-text's upload format (docs/specs/GATEWAY.md, Client
// API → owned parts: speech-to-text): timestamp_granularities[] may repeat, and every
// other part is single-valued.
var transcriptionUpload = &uploadFormat{lists: []string{"timestamp_granularities[]"}, read: readTranscriptionParts}

// readTranscriptionParts reads a transcription's owned parts past model (readUpload),
// in the client's order (docs/specs/GATEWAY.md, Client API → owned parts:
// speech-to-text):
//   - file, the audio: exactly one file part, required — one named twice was refused
//     as the form was read; its bytes pass untouched. Its length is measured here,
//     whatever the model's type (accounting.UploadAudioLength), for the meter: a
//     llama-server answer reports no seconds and counts the measured length — an
//     upload not measured is refused once the model's type is known
//     (checkUploadMeasured) — and a vllm answer reporting none falls back on it;
//   - stream, false or absent (readStreamPart): vLLM streams a transcription in chunks
//     of its own with token usage only, never seconds, and the gateway relays none;
//   - n, the sequences the backend generates (transcriptionSequences), capped by
//     max_n as chat's is: vLLM's beam search takes it as its beam width, its sampling
//     as the sequences it generates, none of them counted;
//   - response_format, read only to know a verbose_json answer, whose duration is the
//     audio's seconds; passed as the client sent it, for the backend to refuse what it
//     cannot answer;
//   - vllm_xargs, refused whatever it holds: vLLM hands its JSON to the model as extra
//     arguments, whose keys the gateway cannot judge;
//   - the input estimate: the prompt's text — the audio is counted in its own unit,
//     seconds.
//
// The request counts toward the audio-processed limits and reserves nothing there: its
// seconds are counted after it, as audio generated is. Everything else — language,
// temperature, timestamp_granularities[], vLLM's hotwords and sampling parts, the
// output-limit parts of either server, … — passes untouched, rebuilt as received.
func readTranscriptionParts(rq *request, parts []provider.FormPart) *apiError {
	file, verbose, prompt := false, false, int64(0)
	var upload time.Duration
	measured := false
	for _, part := range parts {
		var apiErr *apiError
		switch part.Name {
		case "file":
			if !part.File {
				apiErr = errInvalidType("file", "a file part")
			}
			file = true
			upload, measured = accounting.UploadAudioLength(part.Body)
		case "stream":
			apiErr = readStreamPart(part)
		case "n":
			rq.inbound.Sequences, apiErr = transcriptionSequences(part, rq.snapshot.MaxN)
		case "response_format":
			var format string
			format, apiErr = uploadText(part)
			verbose = format == "verbose_json"
		case "vllm_xargs":
			apiErr = errExtraParams("vllm_xargs")
		case "prompt":
			if !part.File {
				prompt = int64(len(part.Body))
			}
		}
		if apiErr != nil {
			return apiErr
		}
	}
	if !file {
		return errMissingFile()
	}
	rq.inbound.Media = map[config.Measure]int64{config.MeasureAudioSecondsIn: 0}
	rq.inbound.Transcription = accounting.Transcription{VerboseJSON: verbose, Upload: upload, UploadMeasured: measured}
	rq.input = accounting.EstimateUploadInput(prompt, 0)
	return nil
}

// transcriptionSequences is the sequences a transcription's n part asks the backend to
// generate (uploadWholeNumber), read as chat reads n (sequences): below 1 counted as 1,
// for the backend to refuse; refused above maxN, max_n, 400 n_too_large.
func transcriptionSequences(part provider.FormPart, maxN int64) (int64, *apiError) {
	n, apiErr := uploadWholeNumber(part, "n")
	if apiErr != nil {
		return 0, apiErr
	}
	if n > maxN {
		return 0, errNTooLarge("n", maxN)
	}
	return max(n, 1), nil
}

// checkUploadMeasured refuses a transcription on a llama-server model whose upload the
// gateway could not measure, 400 audio_format_unsupported (docs/specs/GATEWAY.md,
// Request pipeline, stage 2; Accounting → Media usage: speech-to-text): llama-server
// reports no seconds, so the measured upload is the record's audio_seconds_in, and a
// request whose audio nothing could count is not sent — llama-server decodes WAV,
// FLAC and MP3 alone, so an upload in another format would fail there too, and one in
// those formats left unmeasured is one whose decoded length the gateway cannot vouch
// for. It runs once the model's type is known, after the endpoints list, and before
// limits spend anything on the request. A vllm model's answer reports its seconds: its
// upload needs no measure.
func checkUploadMeasured(_ context.Context, rq *request) *apiError {
	if rq.endpoint.api != provider.AudioTranscriptions || rq.inbound.Transcription.UploadMeasured ||
		rq.snapshot.Models[rq.model].Type() != config.BackendLlamaServer {
		return nil
	}
	return errAudioFormatUnsupported()
}
