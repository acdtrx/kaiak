package server

import (
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
//     as the form was read; its bytes pass untouched;
//   - stream, false or absent (readStreamPart): vLLM streams a transcription in chunks
//     of its own with token usage only, never seconds, and the gateway relays none;
//   - response_format, read only to know a verbose_json answer, whose duration is the
//     audio's seconds; passed as the client sent it, for the backend to refuse what it
//     cannot answer;
//   - vllm_xargs, refused whatever it holds: vLLM hands its JSON to the model as extra
//     arguments, whose keys the gateway cannot judge;
//   - the input estimate: the prompt's text — the audio is counted in its own unit,
//     seconds, from the answer.
//
// The request counts toward the audio-processed limits and reserves nothing there: its
// seconds are known only from its answer. Everything else — language, temperature,
// timestamp_granularities[], vLLM's hotwords and sampling parts, the output-limit
// parts of either server, … — passes untouched, rebuilt as received.
func readTranscriptionParts(rq *request, parts []provider.FormPart) *apiError {
	file, verbose, prompt := false, false, int64(0)
	for _, part := range parts {
		var apiErr *apiError
		switch part.Name {
		case "file":
			if !part.File {
				apiErr = errInvalidType("file", "a file part")
			}
			file = true
		case "stream":
			apiErr = readStreamPart(part)
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
	rq.inbound.Transcription = accounting.Transcription{VerboseJSON: verbose}
	rq.input = accounting.EstimateUploadInput(prompt, 0)
	return nil
}
