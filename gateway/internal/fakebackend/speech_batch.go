package fakebackend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

// The speech batch, as vLLM-Omni serves it (serving_speech.py, create_speech_batch and
// _merge_batch_item; protocol/audio.py; api_server.py, v0.30.0), at
// /v1/audio/speech/batch: each item merged with the batch's defaults — an item's
// value wins, else the batch's — and run as a speech request of its own, its result
// {"index", "status": "success", "audio_data": base64, "media_type", "usage"}, or
// {"index", "status": "error", "error"} for an item the reply fails (FailItems) or
// whose format the fake does not play; all in a JSON body {"id", "results", "total",
// "succeeded", "failed"}, members in vLLM-Omni's order and the absent ones left out
// (exclude_none). An item's audio is speech's whole clip — a WAV file with its real
// sizes, or PCM — AudioMillis long at its rate, its usage the reply's (none with
// OmitUsage). No items, more than speechBatchMaxItems, or a model with no speech
// stage answer 400.

// speechBatchPath is the speech batch endpoint, below the version prefix.
const speechBatchPath = "audio/speech/batch"

// speechBatchMaxItems is vLLM-Omni's tts_batch_max_items default.
const speechBatchMaxItems = 32

// speechBatchResult is one item's result, its members in vLLM-Omni's order
// (SpeechBatchItemResult).
type speechBatchResult struct {
	Index     int            `json:"index"`
	Status    string         `json:"status"`
	AudioData string         `json:"audio_data,omitempty"`
	MediaType string         `json:"media_type,omitempty"`
	Error     string         `json:"error,omitempty"`
	Usage     map[string]any `json:"usage,omitempty"`
}

// speechBatchAnswer is the batch's answer, its members in vLLM-Omni's order
// (BatchSpeechResponse).
type speechBatchAnswer struct {
	ID        string              `json:"id"`
	Results   []speechBatchResult `json:"results"`
	Total     int                 `json:"total"`
	Succeeded int                 `json:"succeeded"`
	Failed    int                 `json:"failed"`
}

// SpeechBatchItemError is the error text of an item the reply fails: a backend's own
// words, which tests prove never reach the gateway's log.
const SpeechBatchItemError = "CUDA error on device 3: the engine died serving Qwen/Qwen3-TTS-backend"

// writeSpeechBatch answers a speech batch for model, each item's usage the reply's.
func writeSpeechBatch(w http.ResponseWriter, reply Reply, top map[string]json.RawMessage, model string, usage Usage,
	nonSpeech []string) {
	badRequest := func(message string) {
		// vLLM's handler renders the route's HTTPException in OpenAI's shape.
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "Bad Request", message))
	}
	if slices.Contains(nonSpeech, model) {
		badRequest("Batch speech is not supported in diffusion mode")
		return
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(top["items"], &items) != nil || len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, omniError(http.StatusBadRequest, "BadRequestError",
			"items: List should have at least 1 item after validation"))
		return
	}
	if len(items) > speechBatchMaxItems {
		badRequest(fmt.Sprintf("Batch contains %d items, exceeding the maximum of %d.", len(items), speechBatchMaxItems))
		return
	}
	answer := speechBatchAnswer{ID: "speech-batch-fake", Total: len(items)}
	for i, item := range items {
		result := speechBatchItem(reply, top, item, usage)
		result.Index = i
		if slices.Contains(reply.FailItems, i) {
			result = speechBatchResult{Index: i, Status: "error", Error: SpeechBatchItemError}
		}
		if result.Status == "success" {
			answer.Succeeded++
		} else {
			answer.Failed++
		}
		answer.Results = append(answer.Results, result)
	}
	writeJSON(w, http.StatusOK, answer)
}

// speechBatchItem runs one item, merged with the batch: its audio — speech's whole
// clip, base64 — and its usage, or an error for a format the fake does not play.
func speechBatchItem(reply Reply, batch, item map[string]json.RawMessage, usage Usage) speechBatchResult {
	pick := func(member string) json.RawMessage {
		if raw, ok := item[member]; ok && string(raw) != "null" {
			return raw
		}
		return batch[member]
	}
	format, rate := "wav", DefaultSampleRate
	_ = json.Unmarshal(pick("response_format"), &format)
	_ = json.Unmarshal(pick("sample_rate"), &rate)
	if format != "wav" && format != "pcm" {
		return speechBatchResult{Status: "error", Error: fmt.Sprintf("the fake does not play response_format %q", format)}
	}
	millis := reply.AudioMillis
	if millis == 0 {
		millis = DefaultAudioMillis
	}
	audio := make([]byte, rate*millis/1000*speechFrameBytes)
	if format == "wav" {
		audio = append(wavHeader(rate, len(audio), false), audio...)
	}
	result := speechBatchResult{Status: "success", AudioData: base64.StdEncoding.EncodeToString(audio), MediaType: "audio/" + format}
	if !reply.OmitUsage {
		result.Usage = map[string]any{
			"input_tokens":        usage.PromptTokens,
			"output_tokens":       usage.CompletionTokens,
			"total_tokens":        usage.PromptTokens + usage.CompletionTokens,
			"input_token_details": map[string]any{"text_tokens": usage.PromptTokens, "audio_tokens": 0},
		}
	}
	return result
}
