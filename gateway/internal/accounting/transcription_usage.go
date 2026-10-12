package accounting

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"kaiak/internal/config"
)

// Transcription is what a speech-to-text request says about its answer, which its
// meter reads (docs/specs/GATEWAY.md, Client API → owned parts: speech-to-text;
// Accounting → Media usage: speech-to-text).
type Transcription struct {
	// VerboseJSON: the request's response_format is verbose_json, whose answer states
	// the audio's length in its top-level duration and reports no usage (vLLM).
	VerboseJSON bool
	// Upload is the uploaded audio's length, when UploadMeasured: what an answer
	// reporting no seconds counts.
	Upload         time.Duration
	UploadMeasured bool
}

// maxDurationBytes bounds the verbose_json duration kept: a number of seconds.
const maxDurationBytes = 64

// transcriptionUsage reads a speech-to-text answer (docs/specs/GATEWAY.md, Accounting →
// Media usage: speech-to-text), a JSON body: vLLM's {"text", "usage": {"type":
// "duration", "seconds"}} (json, text, diarized_json) or, for verbose_json,
// {"duration", "language", "text", "segments", "words"} with no usage; llama-server's
// {"type": "transcript.text.done", "text", "usage": {"type": "tokens", "input_tokens",
// "output_tokens", "total_tokens", "input_tokens_details": {"cached_tokens"}}}. Its
// top-level usage — and a verbose_json answer's duration — are kept as the body
// streams through (memberScanner), never the text, and every member is read by its
// exact name after decoding.
//
// audio_seconds_in is the seconds the answer reports — its usage's seconds, else a
// verbose_json answer's duration — rounded up to a whole second; an answer reporting
// neither (llama-server's, which never does, or one cut short before them) counts the
// upload's measured length, else 0 and the record is estimated. The tokens are a usage
// of type tokens, its cache reads apart (reported); without one the meter estimates
// them — the input estimate, output 0: the answer's text is no more counted than an
// image answer's images are.
type transcriptionUsage struct {
	request Transcription
	body    *memberScanner
}

func newTranscriptionUsage(request Transcription) *transcriptionUsage {
	keep := map[string]int{"usage": maxUsageBytes}
	if request.VerboseJSON {
		keep["duration"] = maxDurationBytes
	}
	return &transcriptionUsage{request: request, body: newMemberScanner(keep)}
}

// streamEvent: a transcription is never a stream — an event stream answering one is no
// answer the reader reads, and it never completes.
func (u *transcriptionUsage) streamEvent([]byte) {}

// contentMember: none — the generated text is not counted.
func (u *transcriptionUsage) contentMember() string { return "" }

func (u *transcriptionUsage) bodyUsage(json.RawMessage) {}

func (u *transcriptionUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *transcriptionUsage) streamContentBytes() int64 { return 0 }

// answered: the answer's headers say nothing the reader reads; its content type is
// not checked — a body that is not a JSON object carries no member the reader keeps.
func (u *transcriptionUsage) answered(http.Header, bool) {}

func (u *transcriptionUsage) bodyPiece(p []byte) { u.body.feed(p) }

// mediaSeen: never — a transcription is a body, never timed to its first token.
func (u *transcriptionUsage) mediaSeen() bool { return false }

// usage is the members of the answer's usage object, read whole; ok is false for an
// answer without one, or whose usage is not an object.
func (u *transcriptionUsage) usage() (map[string]json.RawMessage, bool) {
	raw, ok := u.body.member("usage")
	if !ok {
		return nil, false
	}
	return exactMembers(raw)
}

// reported: llama-server's usage of type tokens, input_tokens and output_tokens both
// integers, mapped as a Responses usage is (inclusiveUnits): input_tokens includes the
// prompt tokens read from the cache, which input_tokens_details reports in
// cached_tokens (and written to it, in cache_write_tokens), so they come out of it;
// output_tokens is tokens_out. Else none: vLLM's usage of type duration, an answer
// without usage, a usage missing either count or holding one that is not an integer,
// or whose details are malformed (cacheDetails). A negative count is 0, as in every
// usage object the gateway reads.
func (u *transcriptionUsage) reported() (Units, bool) {
	usage, ok := u.usage()
	if !ok {
		return nil, true
	}
	if kind, _ := memberValue[string](usage, "type"); kind != "tokens" {
		return nil, true
	}
	in, inOK := memberValue[int64](usage, "input_tokens")
	out, outOK := memberValue[int64](usage, "output_tokens")
	cached, written, detailsOK := cacheDetails(usage)
	if !inOK || !outOK || !detailsOK {
		return nil, true
	}
	units, _ := inclusiveUnits(&in, &out, cached, written, 0)
	return units, true
}

// cacheDetails is the prompt tokens a usage's input_tokens_details reports read from
// and written to the cache — cached_tokens and cache_write_tokens, by their exact
// names — read as a Responses usage's are: details or a count missing or null are 0;
// details that are not an object, or a count that is not an integer, are malformed (ok
// false), and make the usage no report.
func cacheDetails(usage map[string]json.RawMessage) (cached, written int64, ok bool) {
	var details map[string]json.RawMessage
	if raw, present := usage["input_tokens_details"]; present && json.Unmarshal(raw, &details) != nil {
		return 0, 0, false
	}
	count := func(name string) (int64, bool) {
		var n int64
		if raw, present := details[name]; present && json.Unmarshal(raw, &n) != nil {
			return 0, false
		}
		return n, true
	}
	cached, cachedOK := count("cached_tokens")
	written, writtenOK := count("cache_write_tokens")
	return cached, written, cachedOK && writtenOK
}

// media: the seconds the answer reports, else the upload's measured length, else 0,
// unmeasured.
func (u *transcriptionUsage) media(bool) (Units, bool) {
	if seconds, ok := u.answerSeconds(); ok {
		return Units{config.UnitAudioSecondsIn: seconds}, true
	}
	if u.request.UploadMeasured {
		return Units{config.UnitAudioSecondsIn: wholeSeconds(u.request.Upload)}, true
	}
	return Units{config.UnitAudioSecondsIn: 0}, false
}

// answerSeconds is the audio's length the answer reports, rounded up to a whole
// second: its usage's seconds when the usage's type is duration (vLLM's json, text and
// diarized_json), else — for a verbose_json request — its top-level duration. Each is a
// JSON number; ok is false when the answer reports neither.
func (u *transcriptionUsage) answerSeconds() (int64, bool) {
	if usage, ok := u.usage(); ok {
		if kind, _ := memberValue[string](usage, "type"); kind == "duration" {
			if seconds, ok := memberValue[float64](usage, "seconds"); ok {
				return roundedUpSeconds(seconds), true
			}
		}
	}
	if !u.request.VerboseJSON {
		return 0, false
	}
	raw, ok := u.body.member("duration")
	if !ok {
		return 0, false
	}
	var duration *float64
	if json.Unmarshal(raw, &duration) != nil || duration == nil {
		return 0, false
	}
	return roundedUpSeconds(*duration), true
}

// roundedUpSeconds is a length an answer states in seconds as whole seconds, rounded
// up as vLLM rounds its own (int(math.ceil(duration))): a negative length is 0, as a
// negative count is, and one past int64 saturates (the record clamps it to the
// protocol's bound).
func roundedUpSeconds(seconds float64) int64 {
	switch whole := math.Ceil(seconds); {
	case whole <= 0:
		return 0
	case whole >= math.MaxInt64:
		return math.MaxInt64
	default:
		return int64(whole)
	}
}
