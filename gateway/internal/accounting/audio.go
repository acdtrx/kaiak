package accounting

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/bits"
	"mime"
	"strings"
	"time"
)

// Audio length (docs/specs/GATEWAY.md, Accounting → Media usage): audio_seconds_out is
// measured on the audio the backend sent while the client was there, the last piece
// read included — the relay reads each piece before it writes it, so one whose write
// finds the client gone still counts — from the bytes actually sent, never from a size
// a header declares. A clip is read in one pass as its bytes stream through; nothing
// of it is held but a WAV header's fields.

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
	// count reads up to n of the clip's next bytes by their number alone, for audio
	// whose bytes are not at hand (base64 in an event, never decoded whole), and says
	// how many it took: the bytes it does not read — samples, a chunk it skips. It
	// takes none of the bytes it must read (a WAV header's), which come through write.
	count(n int64) int64
}

// pcmFrameBytes is the size of one PCM sample frame vLLM-Omni sends: 16-bit mono,
// the only PCM it can send once sample_rate is required with it
// (docs/specs/GATEWAY.md, Client API → owned fields: speech).
const pcmFrameBytes = 2

// maxAudioByteRate bounds the byte rate of audio the gateway measures: audio said to
// play faster is not measured (0 s, the record estimated), never read as a few
// nanoseconds of audio. Every real format is far below it — 768 kHz, the highest
// sample rate in use, of 32 channels of 32-bit samples is 98.3 MB/s, and the speech
// models vLLM-Omni serves answer 16-bit audio at 44.1 kHz or less, 128 KB/s at most
// (MiniMax Music 3's 32 kHz stereo) — and below it a byte lasts at least 10 ns, so
// rounding each clip down to the nanosecond loses less than one byte's length
// (audioDuration).
const maxAudioByteRate = 100_000_000

// newAudioClip is the clip of an answer whose content type is contentType: audio/wav
// or audio/pcm, as formatClip reads them; any other type cannot be measured.
func newAudioClip(contentType string, sampleRate int64) audioClip {
	return formatClip(audioFormat(contentType), sampleRate)
}

// audioFormat is the format an audio content type names — wav for audio/wav, pcm for
// audio/pcm, as formatClip reads them — or "" for a type that is not audio.
func audioFormat(contentType string) string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	format, ok := strings.CutPrefix(mediaType, "audio/")
	if !ok {
		return ""
	}
	return format
}

// formatClip is a clip of audio in format, as speech's response_format names it: wav,
// read from its own header; pcm, 16-bit mono at sampleRate, the request's (0 when it
// names none, or a rate above maxAudioByteRate: then it cannot be measured); any other
// format cannot be measured.
func formatClip(format string, sampleRate int64) audioClip {
	switch format {
	case "wav":
		return &wavClip{}
	case "pcm":
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
	if c.byteRate <= 0 || c.byteRate > maxAudioByteRate {
		return 0, false
	}
	return audioDuration(c.received, c.byteRate), true
}

func (c *pcmClip) samplesSeen() bool { return c.received > 0 }

func (c *pcmClip) count(n int64) int64 {
	c.received = SaturatingAdd(c.received, n)
	return n
}

// unmeasuredClip is audio the gateway cannot measure: another content type, or PCM at
// no known rate.
type unmeasuredClip struct{}

func (unmeasuredClip) write([]byte)                  {}
func (unmeasuredClip) length() (time.Duration, bool) { return 0, false }
func (unmeasuredClip) samplesSeen() bool             { return false }
func (unmeasuredClip) count(n int64) int64           { return n }

// emptyClip is the audio of an answer that relayed none: 0 s, exactly.
type emptyClip struct{}

func (emptyClip) write([]byte)                  {}
func (emptyClip) length() (time.Duration, bool) { return 0, true }
func (emptyClip) samplesSeen() bool             { return false }
func (emptyClip) count(n int64) int64           { return n }

// wavClip reads a WAV file (RIFF, form type WAVE) as its bytes stream through. Its
// chunks are walked up to the data chunk — the fmt chunk's format, sample rate,
// channels and block size read, any other chunk skipped by its size and pad byte (a
// writer may put more than fmt and data before the audio) — then the data chunk's
// bytes received are its audio, at most the size the chunk declares. A streamed WAV
// declares a placeholder there (vLLM-Omni's raw-audio stream writes 0xFFFFFFFF in both
// size fields), so the length is never read from a size field.
//
// The formats read are those whose data is sample frames of the block size, which
// llama-server's decoder plays (miniaudio's dr_wav, 0.11.25): PCM, IEEE float, A-law
// and μ-law — 8-bit, as dr_wav reads them — and the extensible format whose subformat
// is one of them, named by its GUID's first two bytes as dr_wav reads it (an extension
// of 22 bytes). A WAV whose header cannot be read — not RIFF and WAVE, another format
// (ADPCM's data is blocks of its own), a block size that is not its channels' samples,
// a byte rate above maxAudioByteRate, a data chunk before the fmt chunk, a header cut
// short before its data chunk — is not measured.
type wavClip struct {
	state wavState
	// head holds the bytes of the part being read: the RIFF header, a chunk header,
	// the fmt chunk's fields, the extensible format's extension.
	head []byte
	// skip is how many bytes are left to skip: the rest of a chunk the clip does not
	// read, its pad byte included.
	skip int64
	// fmtSize is the size the fmt chunk declares; format its fields, once read.
	fmtSize int64
	format  wavFormat
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
	// wavFmtExtension: the extensible format's extension — its size, valid bits,
	// channel mask and subformat GUID.
	wavFmtExtension
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
	// wavFmtExtensionBytes: the extensible format's extension past them — its size,
	// then the 22 bytes it declares.
	wavFmtExtensionBytes = 24
	// wavExtensionSize is the size the extensible format's extension declares.
	wavExtensionSize = 22
)

// The format tags whose data is sample frames of the fmt chunk's block size — PCM,
// IEEE float, A-law and μ-law — and the extensible format, whose subformat names one.
const (
	wavFormatPCM        = 1
	wavFormatIEEEFloat  = 3
	wavFormatALaw       = 6
	wavFormatMuLaw      = 7
	wavFormatExtensible = 0xFFFE
)

// wavFormat is a fmt chunk's fields the clip reads.
type wavFormat struct {
	tag, channels, blockSize, sampleBits uint16
	rate                                 uint32
}

func (c *wavClip) write(p []byte) {
	for len(p) > 0 {
		switch c.state {
		case wavRIFF, wavChunkHeader, wavFmt, wavFmtExtension:
			need := c.headBytes()
			n := min(need-len(c.head), len(p))
			c.head = append(c.head, p[:n]...)
			p = p[n:]
			if len(c.head) == need {
				c.readHead()
				c.head = c.head[:0]
			}
		case wavSkip, wavData:
			p = p[c.count(int64(len(p))):]
		default:
			return
		}
	}
}

// count takes the bytes of a chunk the clip skips, and the data chunk's audio up to
// the size it declares; past it, and in a header it cannot read, every byte. A
// header's bytes it does not take.
func (c *wavClip) count(n int64) int64 {
	switch c.state {
	case wavSkip:
		k := min(c.skip, n)
		c.skip -= k
		if c.skip == 0 {
			c.state = wavChunkHeader
		}
		return k
	case wavData:
		k := min(c.declared, n)
		c.declared -= k
		c.received += k
		if c.declared == 0 {
			c.state = wavEnd
		}
		return k
	case wavEnd, wavUnreadable:
		return n
	}
	return 0
}

// headBytes is the size of the fixed part the clip is reading.
func (c *wavClip) headBytes() int {
	switch c.state {
	case wavRIFF:
		return wavRIFFBytes
	case wavChunkHeader:
		return wavChunkHeaderBytes
	case wavFmtExtension:
		return wavFmtExtensionBytes
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
			c.fmtSize = size
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
		c.format = wavFormat{tag: binary.LittleEndian.Uint16(h[0:2]), channels: binary.LittleEndian.Uint16(h[2:4]),
			rate: binary.LittleEndian.Uint32(h[4:8]), blockSize: binary.LittleEndian.Uint16(h[12:14]),
			sampleBits: binary.LittleEndian.Uint16(h[14:16])}
		if c.format.tag != wavFormatExtensible {
			c.readFormat(wavFmtFieldBytes)
			return
		}
		if c.fmtSize < wavFmtFieldBytes+wavFmtExtensionBytes {
			c.state = wavUnreadable
			return
		}
		c.state = wavFmtExtension
	case wavFmtExtension:
		// The extension's size, valid bits (2 bytes), channel mask (4), then the
		// subformat GUID, whose first two bytes are the format tag it names.
		if binary.LittleEndian.Uint16(h[0:2]) != wavExtensionSize {
			c.state = wavUnreadable
			return
		}
		c.format.tag = binary.LittleEndian.Uint16(h[8:10])
		c.readFormat(wavFmtFieldBytes + wavFmtExtensionBytes)
	}
}

// readFormat judges the fmt chunk's format, its fields and extension read (read
// bytes): a format whose data is sample frames, A-law and μ-law of 8-bit samples, a
// rate, channels and samples, a block size that is the channels' samples, a byte rate
// at most maxAudioByteRate. The rest of the chunk is skipped.
func (c *wavClip) readFormat(read int64) {
	f := c.format
	switch {
	case f.tag != wavFormatPCM && f.tag != wavFormatIEEEFloat && f.tag != wavFormatALaw && f.tag != wavFormatMuLaw,
		(f.tag == wavFormatALaw || f.tag == wavFormatMuLaw) && f.sampleBits != 8,
		f.channels == 0 || f.rate == 0 || f.sampleBits == 0,
		int(f.blockSize) != int(f.channels)*((int(f.sampleBits)+7)/8):
		c.state = wavUnreadable
		return
	}
	c.byteRate = int64(f.rate) * int64(f.blockSize)
	if c.byteRate > maxAudioByteRate {
		c.state = wavUnreadable
		return
	}
	c.skipChunk(c.fmtSize - read + c.fmtSize%2)
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

// base64Window is how many bytes of base64 audio are decoded at a time while a clip
// reads them — a WAV header, whose fixed parts are 8 to 24 bytes.
const base64Window = 48

// writeBase64 reads base64 text b64 — a speech.audio.delta's audio — into clip without
// decoding it whole (readBase64). It reports false when bytes the clip must read are
// not base64: the clip cannot read them.
func writeBase64(clip audioClip, b64 string) bool {
	return readBase64(clip, base64Text(b64))
}

// readBase64 reads base64 text without its padding (base64Text) — a whole audio, or a
// run of whole four-character groups of one — into clip without decoding it whole:
// its decoded size from its length (base64Size), the bytes the clip must read (a WAV
// header) decoded a few at a time, the rest taken by their number (audioClip.count).
// It reports false when bytes the clip must read are not base64.
func readBase64[T string | []byte](clip audioClip, text T) bool {
	size := base64Size(text)
	for at := int64(0); at < size; {
		at += clip.count(size - at)
		if at == size {
			break
		}
		n := min(base64Window, size-at)
		p, ok := base64Bytes(text, at, n)
		if !ok {
			return false
		}
		clip.write(p)
		at += n
	}
	return true
}

// base64Text is base64 text without its padding: at most two "=" at its end.
func base64Text[T string | []byte](s T) T {
	for range 2 {
		if n := len(s); n > 0 && s[n-1] == '=' {
			s = s[:n-1]
		}
	}
	return s
}

// base64Size is the decoded size of base64 text without its padding (base64Text):
// three bytes for every four characters, one or two for a last group of two or three.
// A lone character past the last whole group carries no whole byte and counts none.
func base64Size[T string | []byte](text T) int64 {
	size := int64(len(text)/4) * 3
	switch len(text) % 4 {
	case 2:
		size++
	case 3:
		size += 2
	}
	return size
}

// base64Bytes decodes the n bytes at decoded offset at of base64 text without its
// padding, from the four-character groups that hold them alone; ok is false when those
// are not base64.
func base64Bytes[T string | []byte](text T, at, n int64) ([]byte, bool) {
	from, to := at/3*4, min((at+n+2)/3*4, int64(len(text)))
	p, err := base64.RawStdEncoding.DecodeString(string(text[from:to]))
	if skip := at % 3; err == nil && skip+n <= int64(len(p)) {
		return p[skip : skip+n], true
	}
	return nil, false
}

// audioDuration is the length of n bytes of audio at byteRate bytes per second — at
// most maxAudioByteRate, so a byte lasts at least 10 ns — rounded down to the
// nanosecond: less than a nanosecond lost per clip, shorter than any byte of it;
// saturating at the longest time.Duration.
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
