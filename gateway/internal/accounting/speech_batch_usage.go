package accounting

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"kaiak/internal/config"
)

// speechBatchUsage reads a speech batch's answer (docs/specs/GATEWAY.md, Accounting →
// Media usage): a JSON body, {"id", "results": [...], "total", "succeeded",
// "failed"}, each result an item's — {"index", "status": "success", "audio_data":
// base64, "media_type", "usage"}, or {"index", "status": "error", "error"}. The body
// is read in one pass as it streams through, never held: up to 32 items of base64
// audio make answers of many MiB. Each result read whole adds, when its status is
// success, its usage — speech's usage object, as speech.audio.done carries it — and
// its audio's exact length, measured by its media_type at its item's rate; an errored
// one adds nothing. The tokens are the successful items' summed: one without usage
// leaves the batch with no report, as does an answer that relayed no result whole (a
// batch cut short counts the items relayed whole). The seconds are the items' exact
// lengths summed, rounded up once; an item whose audio cannot be measured counts 0
// and leaves the record estimated. The answer generates no text: an estimate's output
// is 0. Members are read from where the answer puts them — the top-level results, each
// result's own members — never searched for by what they look like.
type speechBatchUsage struct {
	// rates are the items' sample rates by position (Speech.ItemSampleRates): a PCM
	// result's audio comes at its item's.
	rates []int64

	// The answer as read so far: the results read whole; the successful ones' tokens
	// summed (untold: one carried no usage) and their audio's exact length summed
	// (unmeasured: one's could not be measured).
	results             int
	tokensIn, tokensOut int64
	untold              bool
	audio               time.Duration
	unmeasured          bool

	phase batchPhase
	// inResult: the object being read is a result, not the answer.
	inResult bool
	// key holds the member name being read, up to maxScanKey bytes; escaped: its
	// last byte began an escape. member is the member whose value comes next.
	key     []byte
	escaped bool
	member  batchMember
	// skip follows the value being skipped — or kept, into keep — and resume is the
	// phase after it.
	skip   valueSkip
	keep   *memberCapture
	resume batchPhase
	// result is the result being read.
	result batchResult
}

// batchPhase is the part of a speech batch answer the reader is in.
type batchPhase int

const (
	// batchStart: before the answer's object.
	batchStart batchPhase = iota
	// batchKey: in an object — the answer or a result — a member's name or its end.
	batchKey
	// batchKeyString: inside a member's name.
	batchKeyString
	// batchColon: after a member's name.
	batchColon
	// batchValue: a member's value.
	batchValue
	// batchAfterValue: after a member's value — the next member or the object's end.
	batchAfterValue
	// batchResults: the results list — a result or the list's end.
	batchResults
	// batchAfterResult: after a result — the next or the list's end.
	batchAfterResult
	// batchAudio: inside a result's audio_data.
	batchAudio
	// batchSkip: inside a value skipped or kept, then resume.
	batchSkip
	// batchDone: the answer's object ended, or the body is not JSON the reader can
	// follow: nothing more is read.
	batchDone
)

// batchMember is a member the reader reads.
type batchMember int

const (
	memberOther batchMember = iota
	memberResults
	memberIndex
	memberStatus
	memberAudioData
	memberMediaType
	memberUsage
)

// The members read, by name: the answer's, and a result's.
var (
	answerMembers = map[string]batchMember{"results": memberResults}
	resultMembers = map[string]batchMember{"index": memberIndex, "status": memberStatus, "audio_data": memberAudioData,
		"media_type": memberMediaType, "usage": memberUsage}
)

// Bounds on the result members kept: an index, a status and a media type are short; a
// usage object is a few hundred bytes (maxUsageBytes).
const (
	maxIndexBytes     = 32
	maxStatusBytes    = 32
	maxMediaTypeBytes = 256
)

// batchResult is the result being read: the members kept, and its audio, measured as
// it passes.
type batchResult struct {
	index, status, mediaType, usage memberCapture
	audio                           itemAudio
	// hasAudio: its audio_data is a string.
	hasAudio bool
}

func newSpeechBatchUsage(request Speech) *speechBatchUsage {
	u := &speechBatchUsage{rates: request.ItemSampleRates}
	u.result.index.limit = maxIndexBytes
	u.result.status.limit = maxStatusBytes
	u.result.mediaType.limit = maxMediaTypeBytes
	u.result.usage.limit = maxUsageBytes
	return u
}

// capture is where member's value is kept; nil for a member not kept.
func (r *batchResult) capture(member batchMember) *memberCapture {
	switch member {
	case memberIndex:
		return &r.index
	case memberStatus:
		return &r.status
	case memberMediaType:
		return &r.mediaType
	case memberUsage:
		return &r.usage
	}
	return nil
}

// reset readies the result for the next one.
func (r *batchResult) reset() {
	for _, c := range []*memberCapture{&r.index, &r.status, &r.mediaType, &r.usage} {
		c.value, c.size, c.complete = c.value[:0], 0, false
	}
	r.hasAudio = false
}

func (u *speechBatchUsage) streamEvent([]byte) {}

// contentMember: none — the answer's content is audio.
func (u *speechBatchUsage) contentMember() string { return "" }

func (u *speechBatchUsage) bodyUsage(json.RawMessage) {}

func (u *speechBatchUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *speechBatchUsage) streamContentBytes() int64 { return 0 }

// answered: the answer's headers say nothing the reader reads. An event stream
// answering a batch is no batch answer: its events are not read.
func (u *speechBatchUsage) answered(http.Header, bool) {}

// mediaSeen: never — a batch answer is a body, never timed to its first audio.
func (u *speechBatchUsage) mediaSeen() bool { return false }

// reported: the successful results' usage summed, once a result was read whole and
// every successful one carried usage.
func (u *speechBatchUsage) reported() (Units, bool) {
	if u.results == 0 || u.untold {
		return nil, true
	}
	return withEveryUnit(Units{config.UnitTokensIn: u.tokensIn, config.UnitTokensOut: u.tokensOut}), true
}

// media: the successful results' audio, its exact length summed and rounded up to
// whole seconds once; unmeasured when one result's could not be measured.
func (u *speechBatchUsage) media() (Units, bool) {
	return Units{config.UnitAudioSecondsOut: wholeSeconds(u.audio)}, !u.unmeasured
}

// bodyPiece reads the answer's next bytes, as relayed.
func (u *speechBatchUsage) bodyPiece(p []byte) {
	for len(p) > 0 && u.phase != batchDone {
		switch u.phase {
		case batchAudio:
			p = u.audioRun(p)
		case batchSkip:
			p = u.skipRun(p)
		default:
			if u.step(p[0]) {
				p = p[1:]
			}
		}
	}
}

// step reads one byte of the answer's structure and reports whether it took it: the
// first byte of a value skipped or kept is the skip's.
func (u *speechBatchUsage) step(c byte) bool {
	switch u.phase {
	case batchStart:
		switch {
		case isJSONSpace(c):
		case c == '{':
			u.phase = batchKey
		default:
			u.phase = batchDone
		}
	case batchKey:
		switch {
		case isJSONSpace(c):
		case c == '"':
			u.phase, u.key, u.escaped = batchKeyString, u.key[:0], false
		case c == '}':
			u.objectEnd()
		default:
			u.phase = batchDone
		}
	case batchKeyString:
		switch {
		case u.escaped:
			u.escaped = false
		case c == '\\':
			u.escaped = true
		case c == '"':
			u.phase, u.member = batchColon, u.wantedMember()
			return true
		}
		if len(u.key) <= maxScanKey {
			u.key = append(u.key, c)
		}
	case batchColon:
		switch {
		case isJSONSpace(c):
		case c == ':':
			u.phase = batchValue
		default:
			u.phase = batchDone
		}
	case batchValue:
		return u.value(c)
	case batchAfterValue:
		switch {
		case isJSONSpace(c):
		case c == ',':
			u.phase = batchKey
		case c == '}':
			u.objectEnd()
		default:
			u.phase = batchDone
		}
	case batchResults:
		switch {
		case isJSONSpace(c):
		case c == ']':
			u.phase = batchAfterValue
		case c == '{':
			u.inResult = true
			u.result.reset()
			u.phase = batchKey
		case c == ',' || c == '}' || c == ':':
			u.phase = batchDone
		default:
			// An element that is not an object is no result.
			u.startSkip(c, nil, batchAfterResult)
			return false
		}
	case batchAfterResult:
		switch {
		case isJSONSpace(c):
		case c == ',':
			u.phase = batchResults
		case c == ']':
			u.phase = batchAfterValue
		default:
			u.phase = batchDone
		}
	}
	return true
}

// value reads the first byte of a member's value: the answer's results list is
// walked; a result's audio_data string measured; its index, status, media_type and
// usage kept; anything else skipped.
func (u *speechBatchUsage) value(c byte) bool {
	switch {
	case isJSONSpace(c):
		return true
	case c == ',' || c == '}' || c == ']' || c == ':':
		u.phase = batchDone
		return true
	}
	switch u.member {
	case memberResults:
		// A client reads the last results an answer names; so does the reader.
		u.restart()
		if c == '[' {
			u.phase = batchResults
			return true
		}
	case memberAudioData:
		u.result.hasAudio = c == '"'
		if u.result.hasAudio {
			u.result.audio.start()
			u.phase = batchAudio
			return true
		}
	}
	u.startSkip(c, u.result.capture(u.member), batchAfterValue)
	return false
}

// objectEnd ends the object being read: a result is finished, and the list goes on;
// the answer's end is the end.
func (u *speechBatchUsage) objectEnd() {
	if !u.inResult {
		u.phase = batchDone
		return
	}
	u.finishResult()
	u.inResult = false
	u.phase = batchAfterResult
}

// restart forgets the results read: the answer names its results again.
func (u *speechBatchUsage) restart() {
	u.results, u.tokensIn, u.tokensOut, u.untold, u.audio, u.unmeasured = 0, 0, 0, false, 0, false
}

// finishResult adds a result read whole: a successful one's usage and audio.
func (u *speechBatchUsage) finishResult() {
	u.results++
	r := &u.result
	if status, ok := decodeCapture[string](&r.status); !ok || status != "success" {
		return
	}
	if units, ok := speechEventUnits(r.usage.whole()); ok {
		u.tokensIn = SaturatingAdd(u.tokensIn, units[config.UnitTokensIn])
		u.tokensOut = SaturatingAdd(u.tokensOut, units[config.UnitTokensOut])
	} else {
		u.untold = true
	}
	if !r.hasAudio {
		return
	}
	mediaType, _ := decodeCapture[string](&r.mediaType)
	var rate int64
	if index, ok := decodeCapture[int64](&r.index); ok && index >= 0 && index < int64(len(u.rates)) {
		rate = u.rates[index]
	}
	d, measured := r.audio.length(mediaType, rate)
	if !measured {
		u.unmeasured = true
		return
	}
	u.audio = time.Duration(SaturatingAdd(int64(u.audio), int64(d)))
}

// wantedMember is the member whose name was just read, at the object's level: the
// answer's or a result's. Names match exactly after decoding.
func (u *speechBatchUsage) wantedMember() batchMember {
	members := answerMembers
	if u.inResult {
		members = resultMembers
	}
	if len(u.key) > maxScanKey {
		return memberOther
	}
	if bytes.IndexByte(u.key, '\\') < 0 {
		return members[string(u.key)]
	}
	var name string
	quoted := append(append([]byte{'"'}, u.key...), '"')
	if json.Unmarshal(quoted, &name) != nil {
		return memberOther
	}
	return members[name]
}

// valueSkip follows a JSON value being skipped: its nesting and strings. A scalar
// (number, true, false, null) ends at the first byte after it.
type valueSkip struct {
	depth                     int
	inString, escaped, scalar bool
}

// startSkip skips the value whose first byte is c — keeping it into keep, unless nil
// — then reads on in resume.
func (u *speechBatchUsage) startSkip(c byte, keep *memberCapture, resume batchPhase) {
	u.skip = valueSkip{scalar: c != '{' && c != '[' && c != '"'}
	u.keep, u.resume, u.phase = keep, resume, batchSkip
	if keep != nil {
		keep.value, keep.size, keep.complete = keep.value[:0], 0, false
	}
}

// skipRun reads the value being skipped from p and returns what follows it once it
// ended: a scalar's end is the byte after it, which is not its own. A string's
// characters are passed over a run at a time.
func (u *speechBatchUsage) skipRun(p []byte) []byte {
	s := &u.skip
	for len(p) > 0 {
		if s.inString && !s.escaped {
			n := stringRun(p)
			u.keepBytes(p[:n])
			if p = p[n:]; len(p) == 0 {
				return p
			}
		}
		c := p[0]
		ended := false
		switch {
		case s.scalar:
			if isJSONSpace(c) || c == ',' || c == '}' || c == ']' {
				u.endSkip()
				return p
			}
		case s.inString:
			switch {
			case s.escaped:
				s.escaped = false
			case c == '\\':
				s.escaped = true
			case c == '"':
				s.inString = false
				ended = s.depth == 0
			}
		default:
			switch c {
			case '"':
				s.inString = true
			case '{', '[':
				s.depth++
			case '}', ']':
				s.depth--
				ended = s.depth == 0
			}
		}
		u.keepBytes(p[:1])
		p = p[1:]
		if ended {
			u.endSkip()
			return p
		}
	}
	return p
}

// keepBytes keeps b, the value's next bytes, when the value is kept: up to its limit,
// every byte counted.
func (u *speechBatchUsage) keepBytes(b []byte) {
	k := u.keep
	if k == nil {
		return
	}
	k.size += len(b)
	if room := k.limit - len(k.value); room > 0 {
		k.value = append(k.value, b[:min(room, len(b))]...)
	}
}

// endSkip ends the value skipped: a kept one is complete when every byte was kept.
func (u *speechBatchUsage) endSkip() {
	if u.keep != nil {
		u.keep.complete = u.keep.size == len(u.keep.value)
		u.keep = nil
	}
	u.phase = u.resume
}

// audioRun reads a result's audio_data string from p — its characters a run at a time
// — and returns what follows it once it ended.
func (u *speechBatchUsage) audioRun(p []byte) []byte {
	a := &u.result.audio
	for len(p) > 0 {
		if a.escaping {
			a.escape(p[0])
			p = p[1:]
			continue
		}
		n := stringRun(p)
		a.write(p[:n])
		if p = p[n:]; len(p) == 0 {
			return p
		}
		if p[0] == '"' {
			a.end()
			u.phase = batchAfterValue
			return p[1:]
		}
		a.escaping = true
		p = p[1:]
	}
	return p
}

// stringRun is how many bytes of p, inside a JSON string, are its characters before
// the next quote or backslash.
func stringRun(p []byte) int {
	end := bytes.IndexByte(p, '"')
	if end < 0 {
		end = len(p)
	}
	if escape := bytes.IndexByte(p[:end], '\\'); escape >= 0 {
		return escape
	}
	return end
}

// whole is the value kept, when it was kept whole; nil otherwise.
func (c *memberCapture) whole() json.RawMessage {
	if !c.complete {
		return nil
	}
	return c.value
}

// decodeCapture decodes a member kept whole; ok is false when it was not, or does not
// decode to a T (null included).
func decodeCapture[T any](c *memberCapture) (T, bool) {
	var v *T
	if !c.complete || json.Unmarshal(c.value, &v) != nil || v == nil {
		var zero T
		return zero, false
	}
	return *v, true
}

// itemAudio measures a batch result's base64 audio as its characters stream through,
// before the result names its format: vLLM-Omni writes media_type after audio_data.
// The audio is read both ways it can be — as a WAV file, its header decoded and its
// data counted (wav), and by its decoded size alone (size), which is PCM's length at
// the item's rate — and the media_type picks one (length). The characters are read a
// window of whole four-character groups at a time, so nothing of the audio is held
// beyond the window, and nothing but a WAV header is decoded.
type itemAudio struct {
	wav wavClip
	// size is the audio's decoded size so far (base64Size).
	size int64
	// wavBad: bytes the WAV header needed were not base64.
	wavBad bool
	// unreadable: the string carried an escape other than \/, so its characters are
	// not at hand; escaping: the last character read began an escape.
	unreadable, escaping bool
	// text holds the n characters not yet read.
	text [itemAudioWindow]byte
	n    int
}

// itemAudioWindow is how many base64 characters an itemAudio holds before it reads
// them: a multiple of four, so a window is whole groups.
const itemAudioWindow = 4 << 10

// start readies the audio for a result's audio_data.
func (a *itemAudio) start() {
	a.wav = wavClip{head: a.wav.head[:0]}
	a.size, a.wavBad, a.unreadable, a.escaping, a.n = 0, false, false, false, 0
}

// write takes the string's next characters. A full window is read only once more
// characters come: the last ones, which may be padding, are read by end.
func (a *itemAudio) write(p []byte) {
	for len(p) > 0 {
		if a.n == len(a.text) {
			a.read(a.text[:])
			a.n = 0
		}
		k := copy(a.text[a.n:], p)
		a.n += k
		p = p[k:]
	}
}

// slash is the character the escape \/ stands for.
var slash = []byte{'/'}

// escape reads the character after a backslash: \/ is a slash, which some encoders
// escape; any other escape has no place in base64, and leaves the audio unmeasured.
func (a *itemAudio) escape(c byte) {
	a.escaping = false
	if c == '/' {
		a.write(slash)
		return
	}
	a.unreadable = true
}

// end reads the string's last characters, without their padding.
func (a *itemAudio) end() {
	a.read(base64Text(a.text[:a.n]))
	a.n = 0
}

// read reads whole groups of characters — or the last of them, padding stripped —
// both ways.
func (a *itemAudio) read(text []byte) {
	a.size = SaturatingAdd(a.size, base64Size(text))
	if !a.wavBad && !readBase64(&a.wav, text) {
		a.wavBad = true
	}
}

// length is the audio's exact length as its media_type names its format: a WAV file's
// from its header and data, PCM's from its size at rate (formatClip); a format the
// gateway cannot measure, PCM at no rate, a WAV header it cannot read and characters
// not at hand are unmeasured.
func (a *itemAudio) length(mediaType string, rate int64) (time.Duration, bool) {
	if a.unreadable {
		return 0, false
	}
	format := audioFormat(mediaType)
	if format == "wav" {
		if a.wavBad {
			return 0, false
		}
		return a.wav.length()
	}
	clip := formatClip(format, rate)
	clip.count(a.size)
	return clip.length()
}
