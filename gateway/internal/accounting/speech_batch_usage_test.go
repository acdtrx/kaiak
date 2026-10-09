package accounting

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// batchUsage is a speech batch result's usage object, as vLLM-Omni writes it.
func batchUsage(in, out int) string {
	return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_token_details":{"text_tokens":%d,"audio_tokens":0}}`,
		in, out, in+out, in)
}

// okResult is a successful result, its members in vLLM-Omni's order: its audio in
// standard base64, padded; usage left out when "".
func okResult(index int, mediaType string, audio []byte, usage string) string {
	r := fmt.Sprintf(`{"index":%d,"status":"success","audio_data":"%s","media_type":"%s"`, index, b64(audio), mediaType)
	if usage != "" {
		r += `,"usage":` + usage
	}
	return r + "}"
}

// errResult is an errored result, as vLLM-Omni writes one.
func errResult(index int) string {
	return fmt.Sprintf(`{"index":%d,"status":"error","error":"CUDA error on device 3: item %d failed"}`, index, index)
}

// batchBody is a speech batch answer holding results.
func batchBody(results ...string) []byte {
	return []byte(`{"id":"speech-batch-1","results":[` + strings.Join(results, ",") + `],"total":` +
		strconv.Itoa(len(results)) + `,"succeeded":0,"failed":0}`)
}

// meterBatch runs a speech batch answer through a meter — answered 200, its body in
// pieces of piece bytes — and settles it.
func meterBatch(speech Speech, body []byte, piece int, complete bool) (Units, Flags) {
	m := NewMeter(provider.AudioSpeechBatch, 33, MediaRequest{Speech: speech})
	m.Answered(http.StatusOK, false, http.Header{"Content-Type": {"application/json"}})
	for len(body) > 0 {
		n := min(piece, len(body))
		m.Observe(provider.Event{Data: body[:n]})
		body = body[n:]
	}
	return m.Settle(complete)
}

// batchLength reads a speech batch answer into a reader, in pieces of piece bytes, and
// returns its audio's exact length and whether every successful result's was measured.
func batchLength(speech Speech, body []byte, piece int) (time.Duration, bool) {
	u := newSpeechBatchUsage(speech)
	for len(body) > 0 {
		n := min(piece, len(body))
		u.bodyPiece(body[:n])
		body = body[n:]
	}
	return u.audio, !u.unmeasured
}

// pieceSizes are the sizes a test splits an answer at: a byte at a time, odd pieces,
// the relay's, whole.
var pieceSizes = []int{1, 3, 4096, 32 << 10, 1 << 30}

// A speech batch's tokens are its successful results' usage summed — each usage
// speech.audio.done's shape — and an errored result adds nothing, its usage unread; a
// successful result without usage, or with one that is not both counts as integers,
// leaves the batch with no report: the input estimated, no output, flagged. An answer
// with no result is no report either. The seconds are the results' exact lengths
// summed, rounded up once — three results of 0.4 s count 2, not 3
// (docs/specs/GATEWAY.md, Accounting → Media usage). Every case reads the same split
// anywhere.
func TestSpeechBatchUsage(t *testing.T) {
	wav := wav16(24000, 19200) // 0.4 s
	ok := func(index int, usage string) string { return okResult(index, "audio/wav", wav, usage) }
	for _, c := range []struct {
		name      string
		body      []byte
		want      Units
		estimated bool
	}{
		{"summed", batchBody(ok(0, batchUsage(7, 40)), ok(1, batchUsage(5, 30)), ok(2, batchUsage(9, 10))),
			speechUnits(21, 80, 2), false},
		{"errored results add nothing", batchBody(ok(0, batchUsage(7, 40)), errResult(1), ok(2, batchUsage(5, 30))),
			speechUnits(12, 70, 1), false},
		{"an errored result's usage is not read", batchBody(ok(0, batchUsage(7, 40)),
			`{"index":1,"status":"error","error":"x","usage":`+batchUsage(100, 100)+`}`), speechUnits(7, 40, 1), false},
		{"every result errored", batchBody(errResult(0), errResult(1)), speechUnits(0, 0, 0), false},
		{"a negative count", batchBody(ok(0, `{"input_tokens":-7,"output_tokens":40}`)), speechUnits(0, 40, 1), false},
		{"a result without usage", batchBody(ok(0, batchUsage(7, 40)), ok(1, "")), speechUnits(33, 0, 1), true},
		{"a result's usage null", batchBody(ok(0, batchUsage(7, 40)), ok(1, "null")), speechUnits(33, 0, 1), true},
		{"a count as text", batchBody(ok(0, `{"input_tokens":"7","output_tokens":40}`)), speechUnits(33, 0, 1), true},
		{"a count missing", batchBody(ok(0, `{"input_tokens":7}`)), speechUnits(33, 0, 1), true},
		{"no results", batchBody(), speechUnits(33, 0, 0), true},
		{"results null", []byte(`{"id":"b","results":null}`), speechUnits(33, 0, 0), true},
		{"not a batch answer", []byte(`[{"index":0,"status":"success"}]`), speechUnits(33, 0, 0), true},
	} {
		for _, piece := range pieceSizes {
			units, flags := meterBatch(Speech{}, c.body, piece, true)
			if !maps.Equal(units, c.want) || flags.Estimated != c.estimated || flags.Partial {
				t.Errorf("%s, pieces of %d: %v %+v, want %v estimated %v", c.name, piece, units, flags, c.want, c.estimated)
			}
		}
	}
}

// Each successful result's audio_data counts by its decoded size — never decoded
// whole — in the format its media_type names, whichever member comes first: a WAV
// file from its own header; PCM at its item's rate, found by the result's index.
// Unpadded base64 and a slash escaped as \/ read the same (docs/specs/GATEWAY.md,
// Accounting → Media usage).
func TestSpeechBatchAudioLength(t *testing.T) {
	const second = time.Second
	wav := wav16(24000, 19200)    // 0.4 s
	pcm8k := make([]byte, 6400)   // 0.4 s at 8 kHz
	pcm16k := make([]byte, 12800) // 0.4 s at 16 kHz
	ff := bytes.Repeat([]byte{0xFF}, 6400)
	rates := Speech{ItemSampleRates: []int64{8000, 16000}}
	usage := batchUsage(7, 40)
	escapeSlashes := func(body []byte) []byte { return bytes.ReplaceAll(body, []byte("/"), []byte(`\/`)) }
	for _, c := range []struct {
		name   string
		speech Speech
		body   []byte
		want   time.Duration
	}{
		{"WAV results, summed exactly", Speech{}, batchBody(okResult(0, "audio/wav", wav, usage), okResult(1, "audio/wav", wav, usage),
			okResult(2, "audio/wav", wav, usage)), 1200 * time.Millisecond},
		{"PCM at each item's rate", rates, batchBody(okResult(0, "audio/pcm", pcm8k, usage), okResult(1, "audio/pcm", pcm16k, usage)),
			800 * time.Millisecond},
		{"PCM rates by index, not by place", rates, batchBody(okResult(1, "audio/pcm", pcm16k, usage),
			okResult(0, "audio/pcm", pcm8k, usage)), 800 * time.Millisecond},
		{"WAV and PCM", rates, batchBody(okResult(0, "audio/pcm", pcm8k, usage), okResult(1, "audio/wav", wav, usage)),
			800 * time.Millisecond},
		{"media_type before audio_data", rates, []byte(`{"results":[{"media_type":"audio/pcm","index":0,"usage":` + usage +
			`,"status":"success","audio_data":"` + b64(pcm8k) + `"}]}`), 400 * time.Millisecond},
		{"media_type with parameters", Speech{}, batchBody(okResult(0, "audio/wav; codecs=1", wav, usage)), 400 * time.Millisecond},
		{"unpadded", rates, []byte(`{"results":[{"index":0,"status":"success","audio_data":"` +
			base64.RawStdEncoding.EncodeToString(make([]byte, 6401)) + `","media_type":"audio/pcm"}]}`), 400*time.Millisecond + 62500},
		{"slashes escaped, PCM", rates, escapeSlashes(batchBody(okResult(0, "audio/pcm", ff, usage))), 400 * time.Millisecond},
		{"slashes escaped, WAV", Speech{}, escapeSlashes(batchBody(okResult(0, "audio/wav",
			riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), wavChunk("data", bytes.Repeat([]byte{0xFF}, 19200))), usage))),
			400 * time.Millisecond},
		{"a WAV whose header holds a slash", Speech{}, escapeSlashes(batchBody(okResult(0, "audio/wav", wav16(16383, 32766), usage))),
			second},
		{"no audio_data", Speech{}, batchBody(`{"index":0,"status":"success","media_type":"audio/wav","usage":`+usage+`}`,
			okResult(1, "audio/wav", wav, usage)), 400 * time.Millisecond},
		{"audio_data null", Speech{}, batchBody(`{"index":0,"status":"success","audio_data":null,"media_type":"audio/wav"}`,
			okResult(1, "audio/wav", wav, usage)), 400 * time.Millisecond},
		{"audio_data named twice: the last", Speech{}, []byte(`{"results":[{"index":0,"status":"success","audio_data":"` +
			b64(wav) + `","audio_data":"` + b64(wav16(24000, 38400)) + `","media_type":"audio/wav"}]}`), 800 * time.Millisecond},
		{"an errored result's audio", Speech{}, batchBody(`{"index":0,"status":"error","audio_data":"`+b64(wav)+
			`","media_type":"audio/wav"}`, okResult(1, "audio/wav", wav, usage)), 400 * time.Millisecond},
		{"a status other than success", Speech{}, batchBody(`{"index":0,"status":"partial","audio_data":"` + b64(wav) +
			`","media_type":"audio/wav"}`), 0},
		// 3070 bytes are 4096 characters, "==" last: the window is full when the string
		// ends, and is read without its padding.
		{"a whole window ending in padding", rates, batchBody(okResult(0, "audio/pcm", make([]byte, 3070), usage)),
			audioDuration(3070, 16000)},
	} {
		for _, piece := range pieceSizes {
			if d, measured := batchLength(c.speech, c.body, piece); d != c.want || !measured {
				t.Errorf("%s, pieces of %d: %v measured %v, want %v measured", c.name, piece, d, measured, c.want)
			}
		}
	}
}

// A successful result whose audio cannot be measured counts 0 and leaves the record
// estimated, its tokens kept; the others still count: PCM at no rate — the item gets
// none, or the result names no index, a negative one or one past the items — a
// format the gateway cannot measure or none named, a WAV header it cannot read or
// that is not base64, an escape base64 has no place for. PCM is counted by its size,
// never decoded: any characters count (docs/specs/GATEWAY.md, Accounting → Media
// usage).
func TestSpeechBatchUnmeasurableAudio(t *testing.T) {
	wav := wav16(24000, 19200) // 0.4 s
	pcm := make([]byte, 6400)  // 0.4 s at 8 kHz
	usage := batchUsage(7, 40)
	measured := okResult(0, "audio/wav", wav, usage)
	for _, c := range []struct {
		name   string
		speech Speech
		result string
	}{
		{"PCM at no rate", Speech{ItemSampleRates: []int64{0, 0}}, okResult(1, "audio/pcm", pcm, usage)},
		{"PCM with no index", Speech{ItemSampleRates: []int64{8000, 8000}},
			`{"status":"success","audio_data":"` + b64(pcm) + `","media_type":"audio/pcm","usage":` + usage + `}`},
		{"PCM with a null index", Speech{ItemSampleRates: []int64{8000, 8000}},
			`{"index":null,"status":"success","audio_data":"` + b64(pcm) + `","media_type":"audio/pcm","usage":` + usage + `}`},
		{"PCM past the items", Speech{ItemSampleRates: []int64{8000, 8000}}, okResult(2, "audio/pcm", pcm, usage)},
		{"PCM with a negative index", Speech{ItemSampleRates: []int64{8000, 8000}}, okResult(-1, "audio/pcm", pcm, usage)},
		{"MP3", Speech{}, okResult(1, "audio/mpeg", pcm, usage)},
		{"no media_type", Speech{}, `{"index":1,"status":"success","audio_data":"` + b64(wav) + `","usage":` + usage + `}`},
		{"a WAV that is not one", Speech{}, okResult(1, "audio/wav", pcm, usage)},
		{"a WAV header that is not base64", Speech{},
			`{"index":1,"status":"success","audio_data":"!!!!` + b64(wav)[4:] + `","media_type":"audio/wav","usage":` + usage + `}`},
		{"an escape", Speech{ItemSampleRates: []int64{8000, 8000}},
			`{"index":1,"status":"success","audio_data":"\u0041` + b64(pcm)[1:] + `","media_type":"audio/pcm","usage":` + usage + `}`},
	} {
		for _, piece := range pieceSizes {
			body := batchBody(measured, c.result)
			if d, ok := batchLength(c.speech, body, piece); d != 400*time.Millisecond || ok {
				t.Errorf("%s, pieces of %d: %v measured %v, want the other result's 400ms, unmeasured", c.name, piece, d, ok)
			}
			if units, flags := meterBatch(c.speech, body, piece, true); !maps.Equal(units, speechUnits(14, 80, 1)) ||
				!flags.Estimated || flags.Partial {
				t.Errorf("%s, pieces of %d: %v %+v, want the tokens and 1 s, estimated", c.name, piece, units, flags)
			}
		}
	}

	// PCM is never decoded: characters base64 has no place for count by their number.
	body := batchBody(`{"index":0,"status":"success","audio_data":"` + strings.Repeat("!", 8532) + `==","media_type":"audio/pcm"}`)
	if d, ok := batchLength(Speech{ItemSampleRates: []int64{8000}}, body, 4096); d != 399937500*time.Nanosecond || !ok {
		t.Errorf("PCM of any characters: %v measured %v, want 399.9375ms (6399 bytes) measured", d, ok)
	}
}

// Members are read where the answer puts them: the top-level results, each result's
// own members — not a member of the same name elsewhere, nested or at the top. Names
// match after decoding; text holding brackets and quotes is passed over; an element
// of the list that is not an object is no result; a results list named twice counts
// as its last, as a client reads it; nothing after the answer's object is read;
// whitespace anywhere reads the same.
func TestSpeechBatchReadsWhereTheAnswerPutsThem(t *testing.T) {
	wav := wav16(24000, 19200) // 0.4 s
	ok := okResult(0, "audio/wav", wav, batchUsage(7, 40))
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"nested look-alikes", batchBody(`{"index":0,"status":"success","extra":{"audio_data":"` + b64(wav) +
			`","media_type":"audio/wav","usage":` + batchUsage(100, 100) + `,"status":"success"},"audio_data":"` + b64(wav) +
			`","media_type":"audio/wav","usage":` + batchUsage(7, 40) + `}`)},
		{"top-level look-alikes", []byte(`{"audio_data":"` + b64(wav) + `","media_type":"audio/wav","status":"success","usage":` +
			batchUsage(100, 100) + `,"results":[` + ok + `]}`)},
		{"escaped names", batchBody(`{"index":0,"st\u0061tus":"success","audio\u005fdata":"` + b64(wav) +
			`","media_type":"audio\/wav","usage":` + batchUsage(7, 40) + `}`)},
		{"brackets and quotes in text", []byte(`{"id":"a\"]}{[","results":[{"index":1,"status":"error","error":"x\"}]{\\"},` + ok +
			`],"note":"]}"}`)},
		{"elements that are not results", batchBody(`null`, `"x"`, `3`, `[1,{"status":"success"}]`, `true`, ok)},
		{"results named twice", []byte(`{"results":[` + ok + `,` + ok + `],"results":[` + ok + `]}`)},
		{"a long name", batchBody(`{"index":0,"status":"success","audio_data` + strings.Repeat("x", 100) + `":"` + b64(wav) +
			`","audio_data":"` + b64(wav) + `","media_type":"audio/wav","usage":` + batchUsage(7, 40) + `}`)},
		{"data after the answer", append(batchBody(ok), `,"results":[`+okResult(0, "audio/wav", wav16(24000, 96000),
			batchUsage(100, 100))+`]}`...)},
	} {
		for _, piece := range pieceSizes {
			units, flags := meterBatch(Speech{}, c.body, piece, true)
			if !maps.Equal(units, speechUnits(7, 40, 1)) || flags.Estimated || flags.Partial {
				t.Errorf("%s, pieces of %d: %v %+v, want one result's 7 and 40 tokens and 1 s", c.name, piece, units, flags)
			}
		}
	}

	var indented bytes.Buffer
	if err := json.Indent(&indented, batchBody(ok, errResult(1), ok), "", "  \t"); err != nil {
		t.Fatal(err)
	}
	if units, flags := meterBatch(Speech{}, indented.Bytes(), 7, true); !maps.Equal(units, speechUnits(14, 80, 1)) ||
		flags.Estimated {
		t.Errorf("indented: %v %+v", units, flags)
	}
}

// A batch cut short counts the results relayed whole — their usage and audio — or
// the estimate when none was, flagged partial (docs/specs/GATEWAY.md, Accounting →
// Partial).
func TestSpeechBatchCutShort(t *testing.T) {
	wav := wav16(24000, 19200) // 0.4 s
	whole := batchBody(okResult(0, "audio/wav", wav, batchUsage(7, 40)), okResult(1, "audio/wav", wav, batchUsage(5, 30)))
	second := bytes.Index(whole, []byte(`{"index":1`))
	closed := bytes.LastIndex(whole, []byte(`]`)) + 1
	for _, c := range []struct {
		name      string
		body      []byte
		want      Units
		estimated bool
	}{
		{"within the first result", whole[:second-200], speechUnits(33, 0, 0), true},
		{"within the second result's audio", whole[:second+100], speechUnits(7, 40, 1), false},
		{"before the second's last brace", whole[:closed-2], speechUnits(7, 40, 1), false},
		{"after the results", whole[:closed], speechUnits(12, 70, 1), false},
	} {
		for _, piece := range pieceSizes {
			units, flags := meterBatch(Speech{}, c.body, piece, false)
			if !maps.Equal(units, c.want) || flags.Estimated != c.estimated || !flags.Partial {
				t.Errorf("%s, pieces of %d: %v %+v, want %v estimated %v, partial", c.name, piece, units, flags, c.want, c.estimated)
			}
		}
	}
}

// An error answer counts nothing; an event stream answering a batch is no batch
// answer — its events are not read, the input estimated (docs/specs/GATEWAY.md,
// Providers: media answers).
func TestSpeechBatchErrorAnswerAndEventStream(t *testing.T) {
	m := NewMeter(provider.AudioSpeechBatch, 33, MediaRequest{})
	m.Answered(http.StatusBadRequest, false, http.Header{"Content-Type": {"application/json"}})
	m.Observe(provider.Event{Data: []byte(`{"error":{"message":"Batch contains 40 items","code":400}}`)})
	if units, flags := m.Settle(true); !maps.Equal(units, speechUnits(0, 0, 0)) || flags.Estimated {
		t.Errorf("an error answer: %v %+v, want zeros", units, flags)
	}

	m = NewMeter(provider.AudioSpeechBatch, 33, MediaRequest{})
	m.Answered(http.StatusOK, true, http.Header{"Content-Type": {"text/event-stream"}})
	payload := batchBody(okResult(0, "audio/wav", wav16(24000, 19200), batchUsage(7, 40)))
	m.Observe(provider.Event{Data: append(append([]byte("data: "), payload...), "\n\n"...), Payload: payload})
	if units, flags := m.Settle(false); !maps.Equal(units, speechUnits(33, 0, 0)) || !flags.Estimated || !flags.Partial {
		t.Errorf("an event stream: %v %+v, want the input estimated, partial", units, flags)
	}
}

// A batch answer of many MiB — up to 32 items of base64 audio — is counted as it
// passes: nothing of it is held, so metering 16 MiB allocates about what metering a
// few KiB does — the bytes, not the count of allocations, which other goroutines'
// make noisy over a longer run (docs/specs/GATEWAY.md, Accounting → Media usage).
func TestSpeechBatchCountsWithoutHolding(t *testing.T) {
	answer := func(dataBytes int) []byte {
		var results []string
		for i := range 4 {
			results = append(results, okResult(i, "audio/wav", wav16(24000, dataBytes), batchUsage(7, 40)))
		}
		return batchBody(results...)
	}
	small, large := answer(4800), answer(3<<20) // 0.1 s, and 65.536 s per result: a body of 16 MiB
	if units, flags := meterBatch(Speech{}, large, 32<<10, true); units[config.UnitAudioSecondsOut] != 263 || flags.Estimated {
		t.Fatalf("large answer: %v %+v, want 263 s (4 × 65.536 s, rounded up once)", units, flags)
	}
	allocated := func(body []byte) uint64 {
		const runs = 3
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			meterBatch(Speech{}, body, 32<<10, true)
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}
	const margin = 256 << 10
	if few, many := allocated(small), allocated(large); many > few+margin {
		t.Errorf("metering allocates %d bytes for an answer of %d, %d for one of %d: it grows with the answer",
			few, len(small), many, len(large))
	}
}
