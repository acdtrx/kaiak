package accounting

import (
	"encoding/binary"
	"math"
	"math/bits"
	"mime"
	"time"
)

// Audio length (docs/specs/GATEWAY.md, Accounting → Media usage): audio_seconds_out is
// measured on the audio relayed to the client, from the bytes actually sent — never
// from a size a header declares. A clip is read in one pass as its bytes stream
// through; nothing of it is held but a WAV header's fields.

// audioClip measures one clip of audio as its bytes stream through.
type audioClip interface {
	// write reads the clip's next bytes, as relayed.
	write(p []byte)
	// length is the audio's exact length so far, rounded down to the nanosecond, and
	// whether it could be measured: false for audio whose format the gateway cannot
	// read.
	length() (d time.Duration, measured bool)
	// samplesSeen reports whether audio samples were relayed — not a WAV header alone.
	samplesSeen() bool
}

// pcmFrameBytes is the size of one PCM sample frame vLLM-Omni sends: 16-bit mono,
// the only PCM it can send once sample_rate is required with it
// (docs/specs/GATEWAY.md, Client API → owned fields: speech).
const pcmFrameBytes = 2

// newAudioClip is the clip of an answer whose content type is contentType: WAV
// (audio/wav), read from its own header; PCM (audio/pcm), 16-bit mono at sampleRate,
// the request's (0 when it names none: then it cannot be measured); any other type
// cannot be measured.
func newAudioClip(contentType string, sampleRate int64) audioClip {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch mediaType {
	case "audio/wav":
		return &wavClip{}
	case "audio/pcm":
		return &pcmClip{byteRate: SaturatingMul(max(sampleRate, 0), pcmFrameBytes)}
	}
	return unmeasuredClip{}
}

// pcmClip is raw PCM at a known byte rate: every byte relayed is audio.
type pcmClip struct {
	byteRate, received int64
}

func (c *pcmClip) write(p []byte) { c.received = SaturatingAdd(c.received, int64(len(p))) }

func (c *pcmClip) length() (time.Duration, bool) {
	if c.byteRate <= 0 {
		return 0, false
	}
	return audioDuration(c.received, c.byteRate), true
}

func (c *pcmClip) samplesSeen() bool { return c.received > 0 }

// unmeasuredClip is audio the gateway cannot measure: another content type, or PCM at
// no known rate.
type unmeasuredClip struct{}

func (unmeasuredClip) write([]byte)                  {}
func (unmeasuredClip) length() (time.Duration, bool) { return 0, false }
func (unmeasuredClip) samplesSeen() bool             { return false }

// wavClip reads a WAV file (RIFF, form type WAVE) as its bytes stream through. Its
// chunks are walked up to the data chunk — the fmt chunk's sample rate, channels and
// block size read, any other chunk skipped by its size and pad byte (a writer may put
// more than fmt and data before the audio) — then the data chunk's bytes received
// are its audio, at most the size the chunk declares. A streamed WAV declares a
// placeholder there (vLLM-Omni's raw-audio stream writes 0xFFFFFFFF in both size
// fields), so the length is never read from a size field. A WAV whose header cannot
// be read — not RIFF and WAVE, a format whose data is not sample frames of its block
// size, a data chunk before the fmt chunk, a header cut short before its data chunk —
// is not measured.
type wavClip struct {
	state wavState
	// head holds the bytes of the part being read: the RIFF header, a chunk header,
	// the fmt chunk's fields.
	head []byte
	// skip is how many bytes are left to skip: the rest of a chunk the clip does not
	// read, its pad byte included.
	skip int64
	// fmtRest is what the fmt chunk holds past its fields, its pad byte included.
	fmtRest int64
	// byteRate is the audio's bytes per second — the fmt chunk's sample rate times its
	// block size (the bytes of one sample frame, every channel); 0 before the fmt chunk.
	byteRate int64
	// declared is how many bytes the data chunk still declares; received the data
	// bytes received.
	declared, received int64
}

// wavState is the part of a WAV file a wavClip is reading.
type wavState int

const (
	// wavRIFF: the RIFF header — "RIFF", the file's size, "WAVE".
	wavRIFF wavState = iota
	// wavChunkHeader: a chunk's ID and size.
	wavChunkHeader
	// wavFmt: the fmt chunk's fields.
	wavFmt
	// wavSkip: a chunk the clip does not read, or the fmt chunk past its fields.
	wavSkip
	// wavData: the data chunk's audio.
	wavData
	// wavEnd: past the data chunk's declared size; the rest is not audio.
	wavEnd
	// wavUnreadable: a header the clip cannot read.
	wavUnreadable
)

// The sizes of a WAV file's fixed parts.
const (
	wavRIFFBytes        = 12
	wavChunkHeaderBytes = 8
	// wavFmtFieldBytes: the format tag, channels, sample rate, byte rate, block size
	// and bits per sample — the fields every fmt chunk holds.
	wavFmtFieldBytes = 16
)

// The format tags whose data is sample frames of the fmt chunk's block size: PCM and
// the extensible format.
const (
	wavFormatPCM        = 1
	wavFormatExtensible = 0xFFFE
)

func (c *wavClip) write(p []byte) {
	for len(p) > 0 {
		switch c.state {
		case wavRIFF, wavChunkHeader, wavFmt:
			need := c.headBytes()
			n := min(need-len(c.head), len(p))
			c.head = append(c.head, p[:n]...)
			p = p[n:]
			if len(c.head) == need {
				c.readHead()
				c.head = c.head[:0]
			}
		case wavSkip:
			n := min(c.skip, int64(len(p)))
			c.skip -= n
			p = p[n:]
			if c.skip == 0 {
				c.state = wavChunkHeader
			}
		case wavData:
			n := min(c.declared, int64(len(p)))
			c.declared -= n
			c.received += n
			p = p[n:]
			if c.declared == 0 {
				c.state = wavEnd
			}
		default:
			return
		}
	}
}

// headBytes is the size of the fixed part the clip is reading.
func (c *wavClip) headBytes() int {
	switch c.state {
	case wavRIFF:
		return wavRIFFBytes
	case wavChunkHeader:
		return wavChunkHeaderBytes
	}
	return wavFmtFieldBytes
}

// readHead reads the fixed part held in head, whole, and moves on to what follows it.
func (c *wavClip) readHead() {
	h := c.head
	switch c.state {
	case wavRIFF:
		if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" {
			c.state = wavUnreadable
			return
		}
		c.state = wavChunkHeader
	case wavChunkHeader:
		size := int64(binary.LittleEndian.Uint32(h[4:8]))
		switch string(h[0:4]) {
		case "fmt ":
			if size < wavFmtFieldBytes {
				c.state = wavUnreadable
				return
			}
			c.fmtRest = size - wavFmtFieldBytes + size%2
			c.state = wavFmt
		case "data":
			if c.byteRate == 0 {
				c.state = wavUnreadable
				return
			}
			c.declared = size
			c.state = wavData
			if size == 0 {
				c.state = wavEnd
			}
		default:
			c.skipChunk(size + size%2)
		}
	case wavFmt:
		tag := binary.LittleEndian.Uint16(h[0:2])
		channels := binary.LittleEndian.Uint16(h[2:4])
		rate := binary.LittleEndian.Uint32(h[4:8])
		blockSize := binary.LittleEndian.Uint16(h[12:14])
		sampleBits := binary.LittleEndian.Uint16(h[14:16])
		if (tag != wavFormatPCM && tag != wavFormatExtensible) || channels == 0 || rate == 0 || sampleBits == 0 ||
			int(blockSize) != int(channels)*((int(sampleBits)+7)/8) {
			c.state = wavUnreadable
			return
		}
		c.byteRate = int64(rate) * int64(blockSize)
		c.skipChunk(c.fmtRest)
	}
}

// skipChunk skips the next n bytes, then reads a chunk header.
func (c *wavClip) skipChunk(n int64) {
	c.skip = n
	c.state = wavSkip
	if n == 0 {
		c.state = wavChunkHeader
	}
}

// length: the data bytes received at the fmt chunk's rate, once the header was read
// through to the data chunk.
func (c *wavClip) length() (time.Duration, bool) {
	if c.state != wavData && c.state != wavEnd {
		return 0, false
	}
	return audioDuration(c.received, c.byteRate), true
}

func (c *wavClip) samplesSeen() bool { return c.received > 0 }

// audioDuration is the length of n bytes of audio at byteRate bytes per second,
// rounded down to the nanosecond — less than a nanosecond lost per clip, which no
// audio byte is shorter than — saturating at the longest time.Duration.
func audioDuration(n, byteRate int64) time.Duration {
	if n <= 0 || byteRate <= 0 {
		return 0
	}
	whole, rest := n/byteRate, n%byteRate
	if whole >= math.MaxInt64/int64(time.Second) {
		return math.MaxInt64
	}
	// rest × 10^9 ÷ byteRate < 10^9: rest < byteRate, so the 128-bit product's high
	// half is below byteRate and the quotient fits.
	hi, lo := bits.Mul64(uint64(rest), uint64(time.Second))
	fraction, _ := bits.Div64(hi, lo, uint64(byteRate))
	return time.Duration(whole)*time.Second + time.Duration(fraction)
}

// wholeSeconds is a record's audio length in whole seconds: rounded up, once per
// record (docs/specs/GATEWAY.md, Accounting → Media usage) — 0 only when no audio
// was relayed.
func wholeSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	seconds := int64(d / time.Second)
	if d%time.Second != 0 {
		seconds++
	}
	return seconds
}
