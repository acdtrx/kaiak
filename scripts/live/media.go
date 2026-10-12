package main

// The speech and image checks, with -speech-base-url and -image-base-url: vLLM-Omni
// servers beside the run's chat backend, each a vllm-omni backend serving one model.
// Audio lengths are measured here from the bytes the client got, and compared with
// the seconds the gateway counted. The media checks' order, the limits and the
// metrics are here too, for image edits (edits.go) and speech-to-text
// (transcription.go) as well.

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

// Request IDs of the media checks' requests whose log lines they read.
const (
	idSpeechWAV     = "live-speech-wav"
	idSpeechPCM     = "live-speech-pcm"
	idSpeechRaw     = "live-speech-raw"
	idSpeechSSE     = "live-speech-sse"
	idSpeechMP3     = "live-speech-mp3"
	idSpeechRefURL  = "live-speech-ref-url"
	idSpeechNoRate  = "live-speech-no-rate"
	idBatch         = "live-speech-batch"
	idBatchCap      = "live-speech-batch-cap"
	idImage         = "live-image"
	idImageCap      = "live-image-cap"
	idImageURL      = "live-image-url"
	idImageFile     = "live-image-file"
	idImageLoRA     = "live-image-lora"
	idImageExtra    = "live-image-extra-params"
	idChatToImage   = "live-chat-to-image"
	idAudioLimit    = "live-audio-limit"
	idAudioRefused  = "live-audio-refused"
	idImageLimit    = "live-image-limit"
	idImageRefused  = "live-image-refused"
	idImageToSpeech = "live-image-to-speech"
	idSpeechToImage = "live-speech-to-image"
)

// speechText and imagePrompt are what the media checks ask for: a sentence of a few
// seconds, a picture.
const (
	speechText  = "The keeper climbed the tower at dusk and lit the lamp."
	imagePrompt = "A lighthouse on a rocky coast at dusk."
)

// pcmSampleRate is the rate the PCM checks ask for, required with pcm: Qwen3-TTS's
// native rate, which Breeze-TTS-2 takes too — the models that accept a sample_rate.
const pcmSampleRate = 24000

// speechBody is a speech request to model for speechText, with extra added.
func speechBody(model string, extra map[string]any) map[string]any {
	return withParams(map[string]any{"model": model, "input": speechText}, extra)
}

// imageBody is an image request to model for one small image, with extra added: few
// steps and a small size — the checks need an image, not a good one.
func imageBody(model string, extra map[string]any) map[string]any {
	return withParams(map[string]any{"model": model, "prompt": imagePrompt, "n": 1, "size": "512x512",
		"num_inference_steps": 10, "seed": 7}, extra)
}

// mediaChecks are the speech, image and edit checks, for the vLLM-Omni servers given,
// and the speech-to-text checks, for a speech-to-text server on a kind serving it:
// each server's own, then the media limits, then each vLLM-Omni model sent the
// other's endpoint, which its endpoints list leaves out.
func (r *run) mediaChecks() {
	speech, image, edit, asr := r.o.speechServer(), r.o.imageServer(), r.o.editServer(), r.o.transcriber()
	if !speech && !image && !edit {
		r.skip("speech, image, edit", "no -speech-base-url, -image-base-url or -edit-base-url")
	} else {
		for _, s := range []struct {
			on         bool
			name, flag string
		}{{speech, "speech", "-speech-base-url"}, {image, "image", "-image-base-url"}, {edit, "edit", "-edit-base-url"}} {
			if !s.on {
				r.skip(s.name, "no "+s.flag)
			}
		}
	}
	switch {
	case r.o.serves(epTranscriptions) && !asr:
		r.skip("transcribe", "no -asr-base-url")
	case !r.o.serves(epTranscriptions) && r.o.asrBaseURL != "":
		r.skip("transcribe", r.o.kind+" serves no speech-to-text: only vllm and llama-server do")
	}
	if !speech && !image && !edit && !asr {
		return
	}
	if speech {
		r.speechChecks()
	}
	if image {
		r.imageChecks()
	}
	if edit {
		r.editChecks()
	}
	if asr {
		r.transcriptionChecks()
	}
	if speech {
		r.checkAudioLimit()
	}
	if image {
		r.checkImageLimit()
	}
	if asr {
		r.checkTranscriptionLimit()
	}
	if speech {
		r.checkImageToSpeech()
	}
	if image {
		r.checkSpeechToImage()
	}
	r.checkMediaMetrics()
}

func (r *run) speechChecks() {
	if clip, ok := r.checkSpeechClip("speech-wav", idSpeechWAV, "wav"); ok {
		r.checkMediaLog("usage-log/speech-wav", idSpeechWAV, mediaLog{outputType: "speech", secondsOut: clip.wholeSeconds()})
	}
	if clip, ok := r.checkSpeechClip("speech-pcm", idSpeechPCM, "pcm"); ok {
		r.checkMediaLog("usage-log/speech-pcm", idSpeechPCM, mediaLog{outputType: "speech", secondsOut: clip.wholeSeconds()})
	}
	if clip, ok := r.checkSpeechRaw(); ok {
		r.checkMediaLog("usage-log/speech-raw", idSpeechRaw, mediaLog{outputType: "speech", stream: true, estimated: true,
			secondsOut: clip.wholeSeconds()})
	}
	if clip, usage, ok := r.checkSpeechSSE(); ok {
		r.checkMediaLog("usage-log/speech-sse", idSpeechSSE, mediaLog{outputType: "speech", stream: true,
			secondsOut: clip.wholeSeconds(), tokens: &usage})
	}
	r.checkSpeechRefused()
	if seconds, usage, ok := r.checkSpeechBatch(); ok {
		r.checkMediaLog("usage-log/batch", idBatch, mediaLog{outputType: "speech", secondsOut: seconds, tokens: &usage})
	}
	r.checkSpeechBatchCap()
}

func (r *run) imageChecks() {
	if images, ok := r.checkImage(); ok {
		r.checkMediaLog("usage-log/image", idImage, mediaLog{outputType: "image", estimated: true, images: images})
	}
	r.checkImageCap()
	r.checkImageRefused()
	r.checkChatToImage()
}

// audioLength is a clip's length as the kit measures it: its audio bytes and the
// bytes one second of it takes.
type audioLength struct {
	bytes, perSecond int64
	format           string
}

// rat is the clip's exact length in seconds.
func (a audioLength) rat() *big.Rat { return big.NewRat(a.bytes, a.perSecond) }

// wholeSeconds is the exact length rounded up to a whole second, as the gateway
// counts a record's audio.
func (a audioLength) wholeSeconds() int64 { return ceilRat(a.rat()) }

func (a audioLength) String() string {
	f, _ := a.rat().Float64()
	return fmt.Sprintf("%s, %.2f s", a.format, f)
}

// ceilRat rounds a non-negative length up to a whole second.
func ceilRat(x *big.Rat) int64 {
	q, m := new(big.Int).QuoRem(x.Num(), x.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// wavLength reads a WAV file's length from its bytes: the fmt chunk's rate and block
// size, then the data chunk's bytes received — at most the size it declares, which a
// streamed WAV leaves as a placeholder.
func wavLength(data []byte) (audioLength, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return audioLength{}, errors.New("not a WAV file: no RIFF/WAVE header")
	}
	var rate, blockAlign int64
	var channels, bits uint16
	rest := data[12:]
	for len(rest) >= 8 {
		id, size := string(rest[:4]), int64(binary.LittleEndian.Uint32(rest[4:8]))
		body := rest[8:]
		switch id {
		case "fmt ":
			if size < 16 || len(body) < 16 {
				return audioLength{}, errors.New("a fmt chunk shorter than 16 bytes")
			}
			if tag := binary.LittleEndian.Uint16(body[0:2]); tag != 1 && tag != 0xFFFE {
				return audioLength{}, fmt.Errorf("format tag %#x: not PCM", tag)
			}
			channels = binary.LittleEndian.Uint16(body[2:4])
			rate = int64(binary.LittleEndian.Uint32(body[4:8]))
			blockAlign = int64(binary.LittleEndian.Uint16(body[12:14]))
			bits = binary.LittleEndian.Uint16(body[14:16])
		case "data":
			if rate == 0 || blockAlign == 0 {
				return audioLength{}, errors.New("no rate or block size before the data chunk")
			}
			return audioLength{bytes: min(size, int64(len(body))), perSecond: rate * blockAlign,
				format: fmt.Sprintf("WAV %d Hz, %d channel(s), %d-bit", rate, channels, bits)}, nil
		}
		skip := size + size%2
		if skip > int64(len(body)) {
			break
		}
		rest = body[skip:]
	}
	return audioLength{}, errors.New("no data chunk")
}

// pcmLength is raw 16-bit mono PCM's length at rate.
func pcmLength(data []byte, rate int64) (audioLength, error) {
	if len(data)%2 != 0 {
		return audioLength{}, fmt.Errorf("%d bytes: not whole 16-bit samples", len(data))
	}
	return audioLength{bytes: int64(len(data)), perSecond: rate * 2, format: fmt.Sprintf("PCM %d Hz, 16-bit mono", rate)}, nil
}

// audioOf measures audio in format ("wav", or "pcm" at pcmSampleRate).
func audioOf(format string, data []byte) (audioLength, error) {
	if format == "pcm" {
		return pcmLength(data, pcmSampleRate)
	}
	return wavLength(data)
}

// imageFormat names an image by its signature: PNG, JPEG or WebP ("" for none).
func imageFormat(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "PNG"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "JPEG"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "WebP"
	}
	return ""
}

// checkSpeechClip asks for the whole clip in format (wav, or pcm with its
// sample_rate): audio of that content type, measured here, and none of the backend's
// x-vllm-omni-* usage headers, which the gateway reads and keeps back.
func (r *run) checkSpeechClip(name, id, format string) (audioLength, bool) {
	extra := map[string]any{"response_format": format}
	if format == "pcm" {
		extra["sample_rate"] = pcmSampleRate
	}
	resp, err := r.post(r.key, "/v1/audio/speech", id, speechBody(modelSpeech, extra))
	if !r.ok(name, resp, err, id) {
		return audioLength{}, false
	}
	contentType := resp.header.Get("Content-Type")
	for h := range resp.header {
		if strings.HasPrefix(strings.ToLower(h), "x-vllm-omni-") {
			r.fail(name, "the backend's usage header %s reached the client", h)
			return audioLength{}, false
		}
	}
	if !strings.HasPrefix(contentType, "audio/"+format) {
		r.fail(name, "content type %q, want audio/%s", contentType, format)
		return audioLength{}, false
	}
	clip, err := audioOf(format, resp.body)
	if err != nil {
		r.fail(name, "%s answer of %d bytes: %v", contentType, len(resp.body), err)
		return audioLength{}, false
	}
	r.pass(name, fmt.Sprintf("%s, %s; no x-vllm-omni-* header", contentType, clip))
	return clip, true
}

// checkSpeechRaw asks for the raw-audio stream (stream_format "audio") in WAV: a
// streamed WAV, its sizes placeholders, measured from the bytes received.
func (r *run) checkSpeechRaw() (audioLength, bool) {
	const name = "speech-raw"
	resp, err := r.post(r.key, "/v1/audio/speech", idSpeechRaw,
		speechBody(modelSpeech, map[string]any{"response_format": "wav", "stream_format": "audio"}))
	if !r.ok(name, resp, err, idSpeechRaw) {
		return audioLength{}, false
	}
	contentType := resp.header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "audio/wav") {
		r.fail(name, "content type %q, want audio/wav", contentType)
		return audioLength{}, false
	}
	clip, err := wavLength(resp.body)
	if err != nil {
		r.fail(name, "raw-audio stream of %d bytes: %v", len(resp.body), err)
		return audioLength{}, false
	}
	r.pass(name, fmt.Sprintf("raw-audio stream, %s", clip))
	return clip, true
}

// speechUsage is the token usage a speech answer reports: speech.audio.done's, or a
// batch item's.
type speechUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// checkSpeechSSE asks for the SSE stream (stream true) in WAV: speech.audio.delta
// events whose audio, joined, is a WAV stream, then speech.audio.done with usage —
// no error event, no [DONE].
func (r *run) checkSpeechSSE() (audioLength, speechUsage, bool) {
	const name = "speech-sse"
	req, err := r.request(r.key, "/v1/audio/speech", idSpeechSSE,
		speechBody(modelSpeech, map[string]any{"response_format": "wav", "stream": true}))
	if err != nil {
		r.fail(name, "%v", err)
		return audioLength{}, speechUsage{}, false
	}
	resp, problem := r.openTypedStream(req, idSpeechSSE)
	if problem != "" {
		r.fail(name, "%s", problem)
		return audioLength{}, speechUsage{}, false
	}
	defer resp.Body.Close()
	events, err := readTypedEvents(resp.Body)
	if err != nil {
		r.fail(name, "%v", err)
		return audioLength{}, speechUsage{}, false
	}
	var audio []byte
	var usage *speechUsage
	deltas := 0
	for i, ev := range events {
		switch ev.Type {
		case "speech.audio.delta":
			chunk, err := base64.StdEncoding.DecodeString(str(ev.Data, "audio"))
			if err != nil || str(ev.Data, "response_format") != "wav" {
				r.fail(name, "delta %d: audio not base64 (%v) or response_format %q", deltas, err, str(ev.Data, "response_format"))
				return audioLength{}, speechUsage{}, false
			}
			audio = append(audio, chunk...)
			deltas++
		case "speech.audio.done":
			var done struct{ Usage *speechUsage }
			if err := json.Unmarshal([]byte(ev.Raw), &done); err != nil || done.Usage == nil || i != len(events)-1 {
				r.fail(name, "speech.audio.done without usage, or not the last event: %s", clip(ev.Raw))
				return audioLength{}, speechUsage{}, false
			}
			usage = done.Usage
		default:
			r.fail(name, "event %q: %s", ev.Type, clip(ev.Raw))
			return audioLength{}, speechUsage{}, false
		}
	}
	if usage == nil {
		r.fail(name, "%d deltas and no speech.audio.done", deltas)
		return audioLength{}, speechUsage{}, false
	}
	length, err := wavLength(audio)
	if err != nil {
		r.fail(name, "%d deltas, %d bytes of audio: %v", deltas, len(audio), err)
		return audioLength{}, speechUsage{}, false
	}
	r.pass(name, fmt.Sprintf("%d deltas, %s; done with %d+%d tokens", deltas, length, usage.InputTokens, usage.OutputTokens))
	return length, *usage, true
}

// refusalCase is a request the gateway must refuse before routing — its body a JSON
// body's members (a map) or an upload's parts ([]formPart) — and the status, code and
// param it answers with.
type refusalCase struct {
	what, path, id string
	body           any
	status         int
	code, param    string
}

// checkRefusals sends each refusal and requires its answer and a log line showing it
// never reached a backend; it passes under name with the list refused.
func (r *run) checkRefusals(name string, refusals []refusalCase) {
	var refused []string
	for _, f := range refusals {
		var resp *response
		var err error
		if parts, upload := f.body.([]formPart); upload {
			resp, err = r.postForm(r.key, f.path, f.id, parts)
		} else {
			resp, err = r.post(r.key, f.path, f.id, f.body)
		}
		if err != nil {
			r.fail(name, "%s: %v", f.what, err)
			return
		}
		var param []string
		if f.param != "" {
			param = []string{f.param}
		}
		if problem := openAIError(resp, f.status, f.code, param...); problem != "" {
			r.fail(name, "%s: %s", f.what, problem)
			return
		}
		if notSent, err := r.notSent(f.id); err != nil || !notSent {
			r.fail(name, "%s: refused, but the log line shows it routed to a backend (%v)", f.what, err)
			return
		}
		refused = append(refused, fmt.Sprintf("%s → %d %s", f.what, f.status, f.code))
	}
	r.pass(name, strings.Join(refused, "; ")+"; none sent")
}

// checkSpeechRefused: a compressed format, reference audio the server would fetch, and
// PCM without its rate are refused before the backend.
func (r *run) checkSpeechRefused() {
	speech := "/v1/audio/speech"
	r.checkRefusals("speech-refused", []refusalCase{
		{"mp3", speech, idSpeechMP3, speechBody(modelSpeech, map[string]any{"response_format": "mp3"}),
			http.StatusBadRequest, "response_format_unsupported", "response_format"},
		{"ref_audio URL", speech, idSpeechRefURL, speechBody(modelSpeech, map[string]any{
			"task_type": "Base", "ref_audio": "https://example.com/voice.wav", "ref_text": "A voice to clone."}),
			http.StatusBadRequest, "external_reference_unsupported", "ref_audio"},
		{"pcm without sample_rate", speech, idSpeechNoRate, speechBody(modelSpeech, map[string]any{"response_format": "pcm"}),
			http.StatusBadRequest, "missing_required_parameter", "sample_rate"},
	})
}

// checkSpeechBatch sends a batch of two items, a WAV and a PCM one: two successful
// results, each item's audio measured by its media_type, and the items' usage and
// exact lengths summed — the record's seconds rounded once.
func (r *run) checkSpeechBatch() (seconds int64, usage speechUsage, ok bool) {
	const name = "speech-batch"
	items := []any{
		map[string]any{"input": speechText},
		map[string]any{"input": "Ships passed safely in the night.", "response_format": "pcm", "sample_rate": pcmSampleRate},
	}
	resp, err := r.post(r.key, "/v1/audio/speech/batch", idBatch,
		map[string]any{"model": modelSpeech, "response_format": "wav", "items": items})
	if !r.ok(name, resp, err, idBatch) {
		return 0, speechUsage{}, false
	}
	var answer struct {
		Results []struct {
			Index     int
			Status    string
			AudioData string `json:"audio_data"`
			MediaType string `json:"media_type"`
			Usage     *speechUsage
			Error     any
		}
	}
	if err := json.Unmarshal(resp.body, &answer); err != nil || len(answer.Results) != len(items) {
		r.fail(name, "want %d results: %s", len(items), clip(string(resp.body)))
		return 0, speechUsage{}, false
	}
	total := new(big.Rat)
	var lengths []string
	for _, res := range answer.Results {
		if res.Status != "success" || res.Usage == nil {
			r.fail(name, "item %d: status %q, usage %v, error %v", res.Index, res.Status, res.Usage, res.Error)
			return 0, speechUsage{}, false
		}
		audio, err := base64.StdEncoding.DecodeString(res.AudioData)
		if err != nil {
			r.fail(name, "item %d: audio_data not base64: %v", res.Index, err)
			return 0, speechUsage{}, false
		}
		format, _ := strings.CutPrefix(res.MediaType, "audio/")
		length, err := audioOf(format, audio)
		if err == nil && format != "wav" && format != "pcm" {
			err = fmt.Errorf("media_type %q", res.MediaType)
		}
		if err != nil {
			r.fail(name, "item %d: %v", res.Index, err)
			return 0, speechUsage{}, false
		}
		total.Add(total, length.rat())
		lengths = append(lengths, length.String())
		usage.InputTokens += res.Usage.InputTokens
		usage.OutputTokens += res.Usage.OutputTokens
	}
	exact, _ := total.Float64()
	r.pass(name, fmt.Sprintf("%s; %.2f s and %d+%d tokens in all", strings.Join(lengths, "; "), exact,
		usage.InputTokens, usage.OutputTokens))
	return ceilRat(total), usage, true
}

// checkSpeechBatchCap sends one item more than global.max_speech_batch_items.
func (r *run) checkSpeechBatchCap() {
	items := make([]any, speechBatchItemsCap+1)
	for i := range items {
		items[i] = map[string]any{"input": fmt.Sprintf("Item %d.", i+1)}
	}
	r.checkRefusals("speech-batch-cap", []refusalCase{{fmt.Sprintf("%d items over max_speech_batch_items %d", len(items),
		speechBatchItemsCap), "/v1/audio/speech/batch", idBatchCap, map[string]any{"model": modelSpeech, "items": items},
		http.StatusBadRequest, "invalid_value", "items"}})
}

// checkImage asks for one image (with the run's imageCheckParams: the self-test asks
// for more): b64_json data whose every element decodes to a PNG, JPEG or WebP. It
// returns how many the answer holds, which a layered model makes more than n.
func (r *run) checkImage() (int64, bool) {
	const name = "image"
	resp, err := r.post(r.key, "/v1/images/generations", idImage, imageBody(modelImage, r.o.imageCheckParams))
	if !r.ok(name, resp, err, idImage) {
		return 0, false
	}
	formats, problem := imagesAnswer(resp.body)
	if problem != "" {
		r.fail(name, "%s", problem)
		return 0, false
	}
	r.pass(name, fmt.Sprintf("%d image(s): %s", len(formats), strings.Join(formats, ", ")))
	return int64(len(formats)), true
}

// imagesAnswer reads an image generation's or edit's answer: its data, each element
// base64 of a PNG, JPEG or WebP. It returns each image's format and size, or what is
// wrong.
func imagesAnswer(body []byte) (formats []string, problem string) {
	var answer struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		}
	}
	if err := json.Unmarshal(body, &answer); err != nil || len(answer.Data) == 0 {
		return nil, "no data: " + clip(string(body))
	}
	for i, d := range answer.Data {
		image, err := base64.StdEncoding.DecodeString(d.B64JSON)
		format := imageFormat(image)
		if err != nil || format == "" {
			return nil, fmt.Sprintf("image %d: not base64 of a PNG, JPEG or WebP (%v)", i, err)
		}
		formats = append(formats, fmt.Sprintf("%s of %d bytes", format, len(image)))
	}
	return formats, ""
}

// checkImageCap asks for one image more than global.max_images_per_request.
func (r *run) checkImageCap() {
	n := imagesPerRequestCap + 1
	r.checkRefusals("image-cap", []refusalCase{{fmt.Sprintf("n %d over max_images_per_request %d", n, imagesPerRequestCap),
		"/v1/images/generations", idImageCap, imageBody(modelImage, map[string]any{"n": n}),
		http.StatusBadRequest, "invalid_value", "n"}})
}

// checkImageRefused: answers other than b64_json, a LoRA the server would load from a
// path, and extra_params, which reach the diffusion pipeline unread by the gateway,
// are refused before the backend.
func (r *run) checkImageRefused() {
	images := "/v1/images/generations"
	r.checkRefusals("image-refused", []refusalCase{
		{"response_format url", images, idImageURL, imageBody(modelImage, map[string]any{"response_format": "url"}),
			http.StatusBadRequest, "response_format_unsupported", "response_format"},
		{"response_format file", images, idImageFile, imageBody(modelImage, map[string]any{"response_format": "file"}),
			http.StatusBadRequest, "response_format_unsupported", "response_format"},
		{"lora", images, idImageLoRA, imageBody(modelImage, map[string]any{"lora": map[string]any{
			"name": "style", "path": "/models/lora/style"}}),
			http.StatusBadRequest, "external_reference_unsupported", "lora"},
		{"extra_params", images, idImageExtra, imageBody(modelImage, map[string]any{"extra_params": map[string]any{}}),
			http.StatusBadRequest, "external_reference_unsupported", "extra_params"},
	})
}

// checkChatToImage: vllm-omni serves the media endpoints alone, so its model's
// endpoints list cannot name chat, and chat to it is the gateway's own refusal, before
// any backend.
func (r *run) checkChatToImage() {
	r.checkRefusals("chat-to-image", []refusalCase{{"chat to " + modelImage, "/v1/chat/completions", idChatToImage,
		r.chatBody(modelImage, false, nil), http.StatusBadRequest, "endpoint_not_served", ""}})
}

// checkMediaLimit sends the same request twice with the media key, whose group allows
// limit of the limit's unit a minute: the first is served and settles; the second is
// refused 429 with errType and Retry-After, no x-ratelimit-* header (media limits send
// none), and a log line naming limitType.
func (r *run) checkMediaLimit(name string, send func(id string) (*response, error), firstID, secondID, errType, limitType string) {
	resp, err := send(firstID)
	if !r.ok(name, resp, err, firstID) {
		return
	}
	// The request line is written after settlement: the window holds the first's usage.
	first, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", firstID))
	if err != nil {
		r.fail(name, "no log line for the first request: %v", err)
		return
	}
	resp, err = send(secondID)
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	var e struct {
		Error struct{ Code, Type string }
	}
	_ = json.Unmarshal(resp.body, &e)
	var ratelimit []string
	for h := range resp.header {
		if strings.HasPrefix(strings.ToLower(h), "x-ratelimit-") {
			ratelimit = append(ratelimit, h)
		}
	}
	switch {
	case resp.status != http.StatusTooManyRequests:
		r.fail(name, "second request: status %d, want 429: %s", resp.status, clip(string(resp.body)))
		return
	case e.Error.Code != "rate_limit_exceeded" || e.Error.Type != errType:
		r.fail(name, "error %s/%s, want %s/rate_limit_exceeded", e.Error.Type, e.Error.Code, errType)
		return
	case resp.header.Get("Retry-After") == "":
		r.fail(name, "no Retry-After")
		return
	case len(ratelimit) > 0:
		r.fail(name, "x-ratelimit headers on a media limit's refusal: %v", ratelimit)
		return
	}
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", secondID))
	if err != nil || line["kaiak.limit.type"] != limitType {
		r.fail(name, "the refusal's log line names limit type %v, want %s (%v)", line["kaiak.limit.type"], limitType, err)
		return
	}
	used := "nothing"
	if v, ok := first["kaiak.usage.images"]; ok {
		used = fmt.Sprintf("%v image(s)", v)
	}
	if v, ok := first["kaiak.usage.audio.output.duration"]; ok {
		used = fmt.Sprintf("%v s of audio", v)
	}
	if v, ok := first["kaiak.usage.audio.input.duration"]; ok {
		used = fmt.Sprintf("%v s of audio processed", v)
	}
	r.pass(name, fmt.Sprintf("first served, counted %s; second → 429 %s, %s %v, Retry-After %s",
		used, errType, limitType, line["kaiak.limit.configured"], resp.header.Get("Retry-After")))
}

// checkAudioLimit: audio generated settles after the request, so a first clip fills a
// 1-second window and the next is refused.
func (r *run) checkAudioLimit() {
	r.checkMediaLimit("audio-limit", func(id string) (*response, error) {
		return r.post(r.mediaKey, "/v1/audio/speech", id, speechBody(modelSpeech, map[string]any{"response_format": "wav"}))
	}, idAudioLimit, idAudioRefused, "audio_seconds", "audio_seconds_out_per_minute")
}

// checkImageLimit: an image request reserves its n, so with 1 image a minute the
// second is refused.
func (r *run) checkImageLimit() {
	r.checkMediaLimit("image-limit", func(id string) (*response, error) {
		return r.post(r.mediaKey, "/v1/images/generations", id, imageBody(modelImage, nil))
	}, idImageLimit, idImageRefused, "images", "images_per_minute")
}

// checkImageToSpeech sends an image request to the speech model, whose endpoints list
// leaves image generation out: the gateway's own 400 endpoint_not_served, never sent
// to the server (docs/specs/GATEWAY.md, Model metadata → Endpoints per model).
func (r *run) checkImageToSpeech() {
	r.checkRefusals("image-to-speech", []refusalCase{{"image to " + modelSpeech, "/v1/images/generations",
		idImageToSpeech, imageBody(modelSpeech, nil), http.StatusBadRequest, "endpoint_not_served", ""}})
}

// checkSpeechToImage sends a speech request to the image model, whose endpoints list
// leaves speech out: the gateway's own 400 endpoint_not_served, never sent to the
// server — where vLLM-Omni would run a whole image generation before answering 400
// (docs/specs/GATEWAY.md, Model metadata → Endpoints per model).
func (r *run) checkSpeechToImage() {
	r.checkRefusals("speech-to-image", []refusalCase{{"speech to " + modelImage, "/v1/audio/speech",
		idSpeechToImage, speechBody(modelImage, nil), http.StatusBadRequest, "endpoint_not_served", ""}})
}

// mediaLog is what a media request's log line must carry.
type mediaLog struct {
	outputType string
	stream     bool
	// estimated: the record's tokens are the gateway's estimate — the raw-audio stream,
	// images and edits, which vLLM-Omni reports none for, and vLLM's transcriptions.
	estimated bool
	// secondsOut, secondsIn and images: the seconds of audio generated and processed
	// and the images the record must count; secondsInFrom says where secondsIn comes
	// from.
	secondsOut, secondsIn, images int64
	secondsInFrom                 string
	// tokens, when set, is the usage the answer reported, which the record must
	// carry exactly; cacheRead is the input tokens of it the answer reported read from
	// the server's cache (llama-server's transcriptions), the record's cache read.
	tokens    *speechUsage
	cacheRead int64
}

// checkMediaLog reads a media request's log line: operation generate_content, its
// output type and stream flag, complete, its tokens — their cache reads among them —
// reported or estimated as want says, and its seconds of audio or images as the kit
// counted them.
func (r *run) checkMediaLog(name, id string, want mediaLog) {
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", id))
	if err != nil {
		r.fail(name, "no log line for request %s: %v", id, err)
		return
	}
	num := func(k string) float64 { v, _ := line[k].(float64); return v }
	secondsOut, secondsIn := num("kaiak.usage.audio.output.duration"), num("kaiak.usage.audio.input.duration")
	images := num("kaiak.usage.images")
	switch {
	case line["http.response.status_code"] != 200.0:
		r.fail(name, "request %s ended with status %v", id, line["http.response.status_code"])
	case line["gen_ai.operation.name"] != "generate_content" || line["gen_ai.output.type"] != want.outputType:
		r.fail(name, "operation %v, output type %v; want generate_content, %s", line["gen_ai.operation.name"],
			line["gen_ai.output.type"], want.outputType)
	case line["gen_ai.request.stream"] != want.stream:
		r.fail(name, "gen_ai.request.stream=%v, want %v", line["gen_ai.request.stream"], want.stream)
	case line["kaiak.usage.partial"] != false:
		r.fail(name, "kaiak.usage.partial=%v (kaiak.relay_end %v)", line["kaiak.usage.partial"], line["kaiak.relay_end"])
	case line["kaiak.usage.estimated"] != want.estimated:
		why := "the backend reported no usage, or audio the gateway could not measure"
		if want.estimated {
			why = "the server reports no token usage here, so the gateway must estimate it"
		}
		r.fail(name, "kaiak.usage.estimated=%v: %s", line["kaiak.usage.estimated"], why)
	case num("gen_ai.usage.input_tokens") == 0:
		r.fail(name, "no input tokens")
	case !want.estimated && want.tokens == nil && num("gen_ai.usage.output_tokens") == 0:
		r.fail(name, "no output tokens")
	case want.tokens != nil && (int64(num("gen_ai.usage.input_tokens")) != want.tokens.InputTokens ||
		int64(num("gen_ai.usage.output_tokens")) != want.tokens.OutputTokens):
		r.fail(name, "tokens %v+%v, the answer reported %d+%d", line["gen_ai.usage.input_tokens"],
			line["gen_ai.usage.output_tokens"], want.tokens.InputTokens, want.tokens.OutputTokens)
	case want.tokens != nil && int64(num("gen_ai.usage.cache_read.input_tokens")) != want.cacheRead:
		r.fail(name, "cache read %v, the answer reported %d of its %d input tokens read from the cache",
			line["gen_ai.usage.cache_read.input_tokens"], want.cacheRead, want.tokens.InputTokens)
	case int64(secondsOut) != want.secondsOut:
		r.fail(name, "kaiak.usage.audio.output.duration=%v s, want %d: the audio measured here, rounded up once", secondsOut,
			want.secondsOut)
	case int64(secondsIn) != want.secondsIn:
		r.fail(name, "kaiak.usage.audio.input.duration=%v s, want %d: %s", secondsIn, want.secondsIn, want.secondsInFrom)
	case int64(images) != want.images:
		r.fail(name, "kaiak.usage.images=%v, want the answer's %d", images, want.images)
	default:
		detail := fmt.Sprintf("in %v (cache read %v), out %v, estimated %v, cost %v", line["gen_ai.usage.input_tokens"],
			line["gen_ai.usage.cache_read.input_tokens"], line["gen_ai.usage.output_tokens"], line["kaiak.usage.estimated"],
			line["kaiak.usage.cost_usd"])
		switch want.outputType {
		case "image":
			r.pass(name, fmt.Sprintf("%d image(s); %s", want.images, detail))
		case "text":
			r.pass(name, fmt.Sprintf("%d s of audio processed, %s; %s", want.secondsIn, want.secondsInFrom, detail))
		default:
			r.pass(name, fmt.Sprintf("%d s of audio, as measured here; %s", want.secondsOut, detail))
		}
	}
}

// checkMediaMetrics reads the admin /metrics: images, edited images, and seconds of
// audio generated and processed under their own counters, by output type, and the
// media limits' refusals.
func (r *run) checkMediaMetrics() {
	const name = "media-metrics"
	status, body, err := httpGet(r.ctx, r.client, r.gw.admin+"/metrics", "")
	if err != nil || status != http.StatusOK {
		r.fail(name, "GET /metrics: %d %v", status, err)
		return
	}
	text := string(body)
	var found []string
	for _, c := range []struct {
		on            bool
		series        string
		labels        []string
		what          string
		wantAtLeastOf float64
	}{
		{r.o.speechServer(), "kaiak_usage_audio_output_duration_seconds_total{",
			[]string{`gen_ai_request_model="` + modelSpeech + `"`, `gen_ai_output_type="speech"`}, "seconds of audio", 1},
		{r.o.speechServer(), "kaiak_limit_rejections_total{", []string{`kaiak_limit_type="audio_seconds_out_per_minute"`},
			"audio limit refusals", 1},
		{r.o.imageServer(), "kaiak_usage_images_total{",
			[]string{`gen_ai_request_model="` + modelImage + `"`, `gen_ai_output_type="image"`}, "images", 1},
		{r.o.imageServer(), "kaiak_limit_rejections_total{", []string{`kaiak_limit_type="images_per_minute"`},
			"image limit refusals", 1},
		{r.o.editServer(), "kaiak_usage_images_total{",
			[]string{`gen_ai_request_model="` + modelEdit + `"`, `gen_ai_output_type="image"`}, "edited images", 1},
		{r.o.transcriber(), "kaiak_usage_audio_input_duration_seconds_total{",
			[]string{`gen_ai_request_model="` + modelASR + `"`, `gen_ai_output_type="text"`}, "seconds of audio processed", 1},
		{r.o.transcriber(), "kaiak_limit_rejections_total{", []string{`kaiak_limit_type="audio_seconds_in_per_minute"`},
			"audio processed limit refusals", 1},
	} {
		if !c.on {
			continue
		}
		v := sumSeries(text, c.series, c.labels...)
		if v < c.wantAtLeastOf {
			r.fail(name, "%s%s} = %v, want ≥ %v", c.series, strings.Join(c.labels, ","), v, c.wantAtLeastOf)
			return
		}
		found = append(found, fmt.Sprintf("%v %s", v, c.what))
	}
	r.pass(name, strings.Join(found, ", "))
}
