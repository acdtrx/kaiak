package accounting

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// wavChunk is a RIFF chunk: its ID, its size and its body, with the pad byte an odd
// size takes.
func wavChunk(id string, body []byte) []byte {
	c := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	c = append(c, body...)
	if len(body)%2 == 1 {
		c = append(c, 0)
	}
	return c
}

// fmtBody is a fmt chunk's fields: format tag, channels, rate, byte rate, block size,
// bits per sample — then extra, the extensible format's bytes past them.
func fmtBody(tag, channels uint16, rate uint32, blockSize, bits uint16, extra ...byte) []byte {
	b := binary.LittleEndian.AppendUint16(nil, tag)
	b = binary.LittleEndian.AppendUint16(b, channels)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*uint32(blockSize))
	b = binary.LittleEndian.AppendUint16(b, blockSize)
	b = binary.LittleEndian.AppendUint16(b, bits)
	return append(b, extra...)
}

// riff is a RIFF WAVE file of chunks, its size field size (the whole when 0).
func riff(size uint32, chunks ...[]byte) []byte {
	var body []byte
	for _, c := range chunks {
		body = append(body, c...)
	}
	if size == 0 {
		size = uint32(4 + len(body))
	}
	f := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, size)...)
	return append(append(f, "WAVE"...), body...)
}

// dataHeader is a data chunk's header declaring size bytes, its audio left to follow.
func dataHeader(size uint32) []byte {
	return append([]byte("data"), binary.LittleEndian.AppendUint32(nil, size)...)
}

// measure writes data to a clip of contentType at sampleRate, in pieces of piece
// bytes (all at once when 0).
func measure(contentType string, sampleRate int64, data []byte, piece int) audioClip {
	c := newAudioClip(contentType, sampleRate)
	if piece == 0 {
		piece = max(len(data), 1)
	}
	for len(data) > 0 {
		n := min(piece, len(data))
		c.write(data[:n])
		data = data[n:]
	}
	return c
}

// A WAV clip's length is its data chunk's bytes received at the fmt chunk's rate and
// block size — never a size field: a whole file with its real sizes, a streamed one
// declaring the placeholder 0xFFFFFFFF, chunks before and between fmt and data (odd
// sizes with their pad byte) walked by their size, the extensible format, stereo and
// 24-bit; at most the size the data chunk declares, so a chunk after it is not
// audio. The result is the same however the bytes are split across reads, a header
// split byte by byte included (docs/specs/GATEWAY.md, Accounting → Media usage).
func TestWAVLengthIsTheDataReceived(t *testing.T) {
	mono16 := wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16))
	audio := func(n int) []byte { return make([]byte, n) }
	for _, c := range []struct {
		name string
		wav  []byte
		want time.Duration
	}{
		{"whole file", riff(0, mono16, wavChunk("data", audio(48000*2+24000))), 2500 * time.Millisecond},
		{"streamed, placeholder sizes", append(riff(0xFFFFFFFF, mono16, dataHeader(0xFFFFFFFF)), audio(72000)...),
			1500 * time.Millisecond},
		{"chunks before and between", riff(0, wavChunk("LIST", []byte("INFOISFTsoundfile")), mono16,
			wavChunk("JUNK", audio(27)), wavChunk("data", audio(48000))), time.Second},
		{"extensible format", riff(0, wavChunk("fmt ", fmtBody(0xFFFE, 2, 48000, 4, 16, make([]byte, 24)...)),
			wavChunk("data", audio(192000/4))), 250 * time.Millisecond},
		{"stereo 24-bit", riff(0, wavChunk("fmt ", fmtBody(1, 2, 44100, 6, 24)), wavChunk("data", audio(44100*6))),
			time.Second},
		{"fmt longer than its fields, odd", riff(0, wavChunk("fmt ", fmtBody(1, 1, 8000, 2, 16, 0, 0, 7)),
			wavChunk("data", audio(16000))), time.Second},
		{"a chunk after the data", riff(0, mono16, wavChunk("data", audio(24000)), wavChunk("LIST", audio(5000))),
			500 * time.Millisecond},
		{"an empty data chunk", riff(0, mono16, wavChunk("data", nil)), 0},
		{"one sample", riff(0, mono16, wavChunk("data", audio(2))), time.Second / 24000},
	} {
		for _, piece := range []int{0, 1, 3, 43, 44, 45, 4096} {
			clip := measure("audio/wav", 0, c.wav, piece)
			got, measured := clip.length()
			if !measured || got != c.want {
				t.Errorf("%s, read %d bytes at a time: %v (measured %v), want %v", c.name, piece, got, measured, c.want)
			}
			if seen := clip.samplesSeen(); seen != (c.want > 0) {
				t.Errorf("%s, read %d bytes at a time: samples seen %v", c.name, piece, seen)
			}
		}
	}
}

// A streamed WAV cut short counts what arrived: a header alone is no samples, and a
// clip cut mid-sample counts its bytes.
func TestWAVCutShortCountsWhatArrived(t *testing.T) {
	head := riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), dataHeader(0xFFFFFFFF))
	clip := measure("audio/wav", 0, head, 0)
	if d, measured := clip.length(); !measured || d != 0 || clip.samplesSeen() {
		t.Errorf("header alone: %v measured %v, samples seen %v; want 0, measured, none", d, measured, clip.samplesSeen())
	}
	clip.write(make([]byte, 24001))
	if d, measured := clip.length(); !measured || d != audioDuration(24001, 48000) || !clip.samplesSeen() {
		t.Errorf("after 24001 bytes: %v measured %v", d, measured)
	}
}

// However a WAV clip's bytes arrive — whole, a byte at a time, or as base64 text read by
// its decoded size, only the header decoded (writeBase64, as a delta's or a batch
// item's audio is read) — its length, whether it is measured and whether samples were
// seen are the same. The seeds walk chunk boundaries: a chunk before fmt, an odd-sized
// one and its pad byte before a data chunk declaring the placeholder, a streamed
// header with audio, a data chunk declaring nothing, a data chunk before any fmt
// chunk, a header cut short (docs/specs/GATEWAY.md, Accounting → Media usage).
func FuzzWAVLengthIsTheSameHoweverRead(f *testing.F) {
	format := wavChunk("fmt ", fmtBody(1, 1, 8000, 2, 16))
	f.Add(riff(0, wavChunk("JUNK", nil), format, wavChunk("data", make([]byte, 32))))
	f.Add(riff(0, wavChunk("JUNK", []byte{1}), format, dataHeader(0xFFFFFFFF)))
	f.Add(append(riff(0xFFFFFFFF, format, dataHeader(0xFFFFFFFF)), make([]byte, 101)...))
	f.Add(riff(0, format, dataHeader(0)))
	f.Add(riff(0, dataHeader(0xFFFFFFFF)))
	f.Add([]byte("RIFF"))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 64<<10 {
			t.Skip()
		}
		whole := measure("audio/wav", 0, body, 0)
		want, measured := whole.length()
		encoded := &wavClip{}
		if !writeBase64(encoded, base64.StdEncoding.EncodeToString(body)) {
			t.Fatal("base64 text refused")
		}
		for name, clip := range map[string]audioClip{"a byte at a time": measure("audio/wav", 0, body, 1), "base64": encoded} {
			if got, ok := clip.length(); got != want || ok != measured || clip.samplesSeen() != whole.samplesSeen() {
				t.Fatalf("%s: %v measured %v samples %v; whole: %v measured %v samples %v", name, got, ok, clip.samplesSeen(),
					want, measured, whole.samplesSeen())
			}
		}
	})
}

// A WAV header the gateway cannot read is not measured: not RIFF and WAVE, a format
// whose data is not sample frames of its block size (IEEE float's tag, a compressed
// one), a block size that is not the channels' samples, a zero rate or channel count,
// a byte rate above any real format's (maxAudioByteRate) — its samples would measure a
// few nanoseconds, 0 s — a fmt chunk shorter than its fields, a data chunk before any
// fmt chunk, a header cut short before its data chunk. A byte rate at the bound is
// measured.
func TestUnreadableWAVIsNotMeasured(t *testing.T) {
	data := wavChunk("data", make([]byte, 4800))
	for name, wav := range map[string][]byte{
		"RIFX":                 append([]byte("RIFX"), riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), data)[4:]...),
		"not WAVE":             append(append([]byte("RIFF\x00\x00\x00\x00AVI "), wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16))...), data...),
		"IEEE float tag":       riff(0, wavChunk("fmt ", fmtBody(3, 1, 24000, 4, 32)), data),
		"MP3 tag":              riff(0, wavChunk("fmt ", fmtBody(0x55, 1, 24000, 1, 0)), data),
		"block size mismatch":  riff(0, wavChunk("fmt ", fmtBody(1, 2, 24000, 2, 16)), data),
		"zero rate":            riff(0, wavChunk("fmt ", fmtBody(1, 1, 0, 2, 16)), data),
		"zero channels":        riff(0, wavChunk("fmt ", fmtBody(1, 0, 24000, 0, 16)), data),
		"an absurd rate":       riff(0, wavChunk("fmt ", fmtBody(1, 8, 0xFFFFFFFF, 32, 32)), data),
		"past the bound":       riff(0, wavChunk("fmt ", fmtBody(1, 1, 50_000_001, 2, 16)), data),
		"short fmt":            riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)[:14]), data),
		"data before fmt":      riff(0, data, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16))),
		"cut in the fmt chunk": riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)))[:30],
		"no data chunk":        riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16))),
		"empty":                nil,
	} {
		clip := measure("audio/wav", 0, wav, 0)
		if d, measured := clip.length(); measured || d != 0 || clip.samplesSeen() {
			t.Errorf("%s: %v measured %v, samples seen %v; want unmeasured", name, d, measured, clip.samplesSeen())
		}
	}
	atBound := riff(0, wavChunk("fmt ", fmtBody(1, 1, 50_000_000, 2, 16)), wavChunk("data", make([]byte, 1000)))
	if d, measured := measure("audio/wav", 0, atBound, 0).length(); !measured || d != 10*time.Microsecond {
		t.Errorf("a byte rate at the bound: %v measured %v, want 10µs", d, measured)
	}
}

// PCM is 16-bit mono at the request's sample rate: every byte is audio. Without a
// rate, or at one past maxAudioByteRate, it is not measured, nor is audio of another
// content type; the content type's parameters do not matter.
func TestPCMAndOtherContentTypes(t *testing.T) {
	clip := measure("audio/pcm", 8000, make([]byte, 24000), 7)
	if d, measured := clip.length(); !measured || d != 1500*time.Millisecond || !clip.samplesSeen() {
		t.Errorf("PCM at 8 kHz, 24000 bytes: %v measured %v, want 1.5s", d, measured)
	}
	if d, measured := measure("audio/pcm", 0, make([]byte, 24000), 0).length(); measured || d != 0 {
		t.Errorf("PCM without a rate: %v measured %v, want unmeasured", d, measured)
	}
	if d, measured := measure("audio/pcm", 50_000_001, make([]byte, 24000), 0).length(); measured || d != 0 {
		t.Errorf("PCM at a rate past the bound: %v measured %v, want unmeasured", d, measured)
	}
	wav := riff(0, wavChunk("fmt ", fmtBody(1, 1, 24000, 2, 16)), wavChunk("data", make([]byte, 48000)))
	if d, measured := measure("audio/wav; codecs=1", 0, wav, 0).length(); !measured || d != time.Second {
		t.Errorf("audio/wav with a parameter: %v measured %v", d, measured)
	}
	for _, contentType := range []string{"audio/mpeg", "audio/x-wav", "application/json", "text/event-stream", ""} {
		clip := measure(contentType, 24000, wav, 0)
		if d, measured := clip.length(); measured || d != 0 || clip.samplesSeen() {
			t.Errorf("%q: %v measured %v, want unmeasured", contentType, d, measured)
		}
	}
}

// The exact length rounds down to the nanosecond and saturates; a record's seconds
// round up once, 0 only for no audio.
func TestAudioDurationAndWholeSeconds(t *testing.T) {
	for _, c := range []struct {
		n, byteRate int64
		want        time.Duration
	}{
		{0, 48000, 0},
		{1, 48000, 20833},
		{48000, 48000, time.Second},
		{48001, 48000, time.Second + 20833},
		{3, 0, 0},
		{math.MaxInt64, 1, math.MaxInt64},
		// The fraction's product passes 2^64: the 128-bit division keeps it exact.
		{3<<48 + 1<<47, 1 << 48, 3500 * time.Millisecond},
	} {
		if got := audioDuration(c.n, c.byteRate); got != c.want {
			t.Errorf("audioDuration(%d, %d) = %d, want %d", c.n, c.byteRate, got, c.want)
		}
	}
	for d, want := range map[time.Duration]int64{
		0: 0, 1: 1, time.Second: 1, 2 * time.Second: 2, 2*time.Second + 1: 3, 2500 * time.Millisecond: 3,
		math.MaxInt64: int64(math.MaxInt64/time.Second) + 1,
	} {
		if got := wholeSeconds(d); got != want {
			t.Errorf("wholeSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}
