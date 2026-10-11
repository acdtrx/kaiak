package accounting

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/big"
	"testing"
	"time"
)

// pcmWAV is a PCM WAV file — channels of bits-bit samples at rate — whose data chunk
// holds n bytes, chunks before it as given.
func pcmWAV(channels uint16, rate uint32, bits uint16, n int, before ...[]byte) []byte {
	block := channels * ((bits + 7) / 8)
	chunks := append(before, wavChunk("fmt ", fmtBody(1, channels, rate, block, bits)), wavChunk("data", filler(n)))
	return riff(0, chunks...)
}

// tagWAV is a WAV file of format tag — channels of bits-bit samples at rate, the fmt
// chunk's bytes past its fields extra (an extensible format's extension) — whose data
// chunk holds n bytes, a fact chunk before it, as writers of these formats put one.
func tagWAV(tag, channels uint16, rate uint32, bits uint16, n int, extra ...byte) []byte {
	block := channels * ((bits + 7) / 8)
	return riff(0, wavChunk("fmt ", fmtBody(tag, channels, rate, block, bits, extra...)), wavChunk("fact", filler(4)),
		wavChunk("data", filler(n)))
}

// filler is n bytes of audio: every byte value in turn, 0xFF and the bytes of a frame
// sync among them.
func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// lengthOf is the length of n bytes at byteRate bytes a second, rounded down to the
// nanosecond — computed apart from the readers, with big integers.
func lengthOf(n, byteRate int64) time.Duration {
	ns := new(big.Int).Mul(big.NewInt(n), big.NewInt(int64(time.Second)))
	return time.Duration(ns.Div(ns, big.NewInt(byteRate)).Int64())
}

// A WAV upload's length is its data chunk's bytes present at the fmt chunk's byte rate
// — rate × channels × bytes a sample, computed here from the test's own numbers — for
// every format llama-server's decoder plays: PCM of 8, 16, 24 and 32-bit samples, mono
// and stereo; IEEE float of 32 and 64-bit samples; 8-bit A-law and μ-law; the
// extensible format naming each of them in its 22-byte extension. Chunks before the data, odd ones with their pad byte; a data chunk
// declaring more than the file holds, or the streamed placeholder, counts the bytes
// present, and one declaring less counts what it declares (docs/specs/GATEWAY.md,
// Accounting → Media usage: speech-to-text).
func TestWAVUploadLength(t *testing.T) {
	declaring := func(declared uint32, present int) []byte {
		f := riff(0, wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16)), dataHeader(declared))
		return append(f, filler(present)...)
	}
	for _, c := range []struct {
		name     string
		file     []byte
		byteRate int64
		data     int64
	}{
		{"8-bit mono", pcmWAV(1, 8000, 8, 12000), 8000, 12000},
		{"16-bit mono", pcmWAV(1, 16000, 16, 32000*3+5), 32000, 32000*3 + 5},
		{"16-bit stereo", pcmWAV(2, 44100, 16, 44100*4*2), 44100 * 4, 44100 * 4 * 2},
		{"24-bit mono", pcmWAV(1, 48000, 24, 48000*3/4), 48000 * 3, 48000 * 3 / 4},
		{"24-bit stereo", pcmWAV(2, 22050, 24, 22050*6+18), 22050 * 6, 22050*6 + 18},
		{"32-bit mono", pcmWAV(1, 16000, 32, 64000*7), 64000, 64000 * 7},
		{"32-bit stereo", pcmWAV(2, 8000, 32, 64000*2+64), 64000, 64000*2 + 64},
		{"IEEE float 32-bit mono", tagWAV(3, 1, 16000, 32, 64000*3+4, 0, 0), 16000 * 1 * 4, 64000*3 + 4},
		{"IEEE float 32-bit stereo", tagWAV(3, 2, 44100, 32, 44100*8/2), 44100 * 2 * 4, 44100 * 8 / 2},
		{"IEEE float 64-bit mono", tagWAV(3, 1, 22050, 64, 22050*8*5), 22050 * 1 * 8, 22050 * 8 * 5},
		{"IEEE float 64-bit stereo", tagWAV(3, 2, 48000, 64, 48000*16+16, 0, 0), 48000 * 2 * 8, 48000*16 + 16},
		{"A-law mono", tagWAV(6, 1, 8000, 8, 8000*7+1, 0, 0), 8000, 8000*7 + 1},
		{"μ-law stereo", tagWAV(7, 2, 8000, 8, 16000*3, 0, 0), 8000 * 2, 16000 * 3},
		{"extensible PCM", tagWAV(0xFFFE, 2, 48000, 24, 48000*6, extension(24, 1)...), 48000 * 2 * 3, 48000 * 6},
		{"extensible IEEE float", tagWAV(0xFFFE, 6, 48000, 32, 48000*24/4, extension(32, 3)...), 48000 * 6 * 4,
			48000 * 24 / 4},
		{"extensible A-law", tagWAV(0xFFFE, 1, 8000, 8, 8000*2, extension(8, 6)...), 8000, 8000 * 2},
		{"extensible μ-law", tagWAV(0xFFFE, 1, 16000, 8, 16000+3, extension(8, 7)...), 16000, 16000 + 3},
		{"odd chunks before the data", pcmWAV(1, 16000, 16, 32000, wavChunk("LIST", []byte("INFOISFTLavf61")),
			wavChunk("JUNK", filler(27)), wavChunk("bext", filler(3))), 32000, 32000},
		{"a chunk between fmt and data", riff(0, wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16)), wavChunk("fact", filler(4)),
			wavChunk("data", filler(16000))), 32000, 16000},
		{"declared past the end", declaring(10<<20, 48000), 32000, 48000},
		{"declared past the end, odd", declaring(0x7FFFFFFF, 333), 32000, 333},
		{"the streamed placeholder", declaring(0xFFFFFFFF, 64000), 32000, 64000},
		{"declared less than present", declaring(16000, 64000), 32000, 16000},
		{"an empty data chunk", pcmWAV(1, 16000, 16, 0), 32000, 0},
	} {
		got, measured := UploadAudioLength(c.file)
		if want := lengthOf(c.data, c.byteRate); !measured || got != want {
			t.Errorf("%s: %v (measured %v), want %v", c.name, got, measured, want)
		}
	}
}

// A WAV header the gateway cannot read is not measured, as audio generated is not: a
// format llama-server's decoder does not play as sample frames — ADPCM, MP3, an
// unknown tag — or the extensible format naming one, or naming none; A-law or μ-law of
// samples wider than 8 bits; a header cut short, a data chunk before the fmt chunk, a
// zero rate, a chunk declaring more than the file holds before the data, RIFF of
// another form type (docs/specs/GATEWAY.md, Accounting → Media usage).
func TestUnmeasurableWAVUploads(t *testing.T) {
	format := wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16))
	data := wavChunk("data", filler(3200))
	for name, file := range map[string][]byte{
		"ADPCM":                      riff(0, wavChunk("fmt ", fmtBody(2, 1, 16000, 256, 4, 2, 0, 0xF4, 0x01)), data),
		"MP3 in WAV":                 riff(0, wavChunk("fmt ", fmtBody(0x55, 1, 16000, 1, 0)), data),
		"an unknown tag":             tagWAV(0x1234, 1, 16000, 16, 3200),
		"extensible of an unknown":   tagWAV(0xFFFE, 1, 16000, 16, 3200, extension(16, 0x1234)...),
		"extensible of ADPCM":        tagWAV(0xFFFE, 1, 16000, 16, 3200, extension(16, 2)...),
		"extensible of extensible":   tagWAV(0xFFFE, 1, 16000, 16, 3200, extension(16, 0xFFFE)...),
		"extensible, size 0":         tagWAV(0xFFFE, 1, 16000, 16, 3200, append([]byte{0, 0}, extension(16, 1)[2:]...)...),
		"extensible, no extension":   tagWAV(0xFFFE, 1, 16000, 16, 3200),
		"16-bit μ-law":               tagWAV(7, 1, 16000, 16, 3200),
		"IEEE float, a short block":  riff(0, wavChunk("fmt ", fmtBody(3, 2, 16000, 4, 32)), data),
		"cut in the fmt chunk":       riff(0, format)[:30],
		"no data chunk":              riff(0, format),
		"data before fmt":            riff(0, data, format),
		"zero rate":                  riff(0, wavChunk("fmt ", fmtBody(1, 1, 0, 2, 16)), data),
		"a chunk past the end":       append(riff(0, format), "LIST\xff\xff\xff\x7fINFO"...),
		"a zero-size chunk then end": riff(0, format, wavChunk("JUNK", nil)),
		"AVI":                        append(append([]byte("RIFF\x00\x00\x00\x00AVI "), format...), data...),
		"RIFF alone":                 []byte("RIFF\x24\x00\x00\x00WAVE"),
	} {
		if d, measured := UploadAudioLength(file); measured || d != 0 {
			t.Errorf("%s: %v measured %v, want unmeasured", name, d, measured)
		}
	}
}

// flacStream is a FLAC stream: fLaC, then STREAMINFO — rate, channels, bits per sample
// and total samples packed as the format packs them — then frames, whose bytes the
// reader never reads: a frame sync and filler.
func flacStream(rate uint32, channels, bits uint8, total uint64) []byte {
	info := binary.BigEndian.AppendUint16(nil, 4096)
	info = binary.BigEndian.AppendUint16(info, 4096)
	info = append(info, 0, 0, 14, 0, 0x30, 0x00) // min and max frame sizes, 24 bits each
	props := uint64(rate)<<44 | uint64(channels-1)<<41 | uint64(bits-1)<<36 | total
	info = binary.BigEndian.AppendUint64(info, props)
	info = append(info, make([]byte, 16)...) // MD5
	b := append([]byte("fLaC"), 0x80, 0, 0, byte(len(info)))
	b = append(b, info...)
	return append(append(b, 0xFF, 0xF8), filler(300)...)
}

// id3Tag is an ID3v2.4 tag holding body, its size in four 7-bit bytes, with a footer
// when footer is set.
func id3Tag(body []byte, footer bool) []byte {
	n := len(body)
	flags := byte(0)
	if footer {
		flags = 0x10
	}
	tag := append([]byte("ID3\x04\x00"), flags, byte(n>>21&0x7F), byte(n>>14&0x7F), byte(n>>7&0x7F), byte(n&0x7F))
	tag = append(tag, body...)
	if footer {
		tag = append(tag, append([]byte("3DI\x04\x00"), tag[5:10]...)...)
	}
	return tag
}

// A FLAC upload's length is STREAMINFO's total samples over its sample rate, whatever
// the frames hold — also past ID3v2 tags, which llama-server's FLAC decoder skips
// (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).
func TestFLACUploadLength(t *testing.T) {
	tagged := append(id3Tag(append([]byte("TIT2\x00\x00\x00\x05\x00\x00\x03Kaiak"), 0xFF, 0xFB, 0x90, 0x64), false),
		id3Tag([]byte("TPE1\x00\x00\x00\x02\x00\x00\x03K"), true)...)
	for _, c := range []struct {
		name  string
		file  []byte
		total int64
		rate  int64
	}{
		{"stereo 16-bit", flacStream(44100, 2, 16, 441000), 441000, 44100},
		{"mono 24-bit", flacStream(48000, 1, 24, 120000), 120000, 48000},
		{"one sample", flacStream(96000, 1, 16, 1), 1, 96000},
		{"the largest total at the largest rate", flacStream(1<<20-1, 8, 32, 1<<36-1), 1<<36 - 1, 1<<20 - 1},
		{"past ID3v2 tags", append(tagged, flacStream(16000, 1, 16, 16000*7+3)...), 16000*7 + 3, 16000},
	} {
		got, measured := UploadAudioLength(c.file)
		if want := lengthOf(c.total, c.rate); !measured || got != want {
			t.Errorf("%s: %v (measured %v), want %v", c.name, got, measured, want)
		}
	}
}

// A FLAC stream whose STREAMINFO does not give a length is not measured: a total of 0
// ("unknown"), a rate of 0, a first block that is not STREAMINFO or not of its size,
// a block cut short, a length past the longest time.Duration; nor is an ID3v2 tag
// running past the file's end.
func TestUnmeasurableFLACUploads(t *testing.T) {
	good := flacStream(44100, 2, 16, 44100)
	with := func(at int, value byte) []byte {
		b := bytes.Clone(good)
		b[at] = value
		return b
	}
	for name, file := range map[string][]byte{
		"total 0":                 flacStream(44100, 2, 16, 0),
		"rate 0":                  flacStream(0, 2, 16, 44100),
		"first block not info":    with(4, 0x84),
		"info of 33 bytes":        with(7, 33),
		"info of 35 bytes":        with(7, 35),
		"cut in the info":         good[:30],
		"fLaC alone":              []byte("fLaC"),
		"past the longest length": flacStream(1, 1, 16, 1<<36-1),
		"a tag past the end":      append(id3Tag(nil, false)[:6], 0, 0, 1, 0, 'f', 'L', 'a', 'C'),
	} {
		if d, measured := UploadAudioLength(file); measured || d != 0 {
			t.Errorf("%s: %v measured %v, want unmeasured", name, d, measured)
		}
	}
}

// A file in a format llama-server does not decode is not measured, whatever it holds:
// M4A, Ogg, WebM, text, nothing (docs/specs/GATEWAY.md, Accounting → Media usage:
// speech-to-text).
func TestOtherFormatsAreNotMeasured(t *testing.T) {
	for name, file := range map[string][]byte{
		"M4A":     append([]byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom"), filler(400)...),
		"Ogg":     append([]byte("OggS\x00\x02"), filler(400)...),
		"WebM":    append([]byte("\x1a\x45\xdf\xa3"), filler(400)...),
		"text":    []byte("RIFF is not this file's first word"),
		"nothing": nil,
		"a byte":  {0xFF},
	} {
		if d, measured := UploadAudioLength(file); measured || d != 0 {
			t.Errorf("%s: %v measured %v, want unmeasured", name, d, measured)
		}
	}
}

// However hostile a WAV upload, its pass ends, never panics, and its data is at most
// the bytes the file holds past the headers it read — the RIFF header, the fmt and data
// chunk headers and the fmt chunk's fields — at the fmt chunk's byte rate.
func FuzzWAVUploadLength(f *testing.F) {
	f.Add(pcmWAV(2, 44100, 16, 400))
	f.Add(pcmWAV(1, 8000, 8, 31, wavChunk("JUNK", filler(3))))
	f.Add(append(riff(0xFFFFFFFF, wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16)), dataHeader(0xFFFFFFFF)), filler(77)...))
	f.Add(riff(0, wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16)), dataHeader(0x7FFFFFFF)))
	f.Add(append(riff(0, wavChunk("fmt ", fmtBody(1, 1, 16000, 2, 16))), "LIST\xff\xff\xff\xff"...))
	f.Add(tagWAV(3, 2, 48000, 64, 96))
	f.Add(tagWAV(7, 1, 8000, 8, 33))
	f.Add(tagWAV(0xFFFE, 2, 44100, 32, 64, extension(32, 3)...))
	f.Add(tagWAV(0xFFFE, 1, 8000, 8, 9, extension(8, 6)...))
	f.Add(tagWAV(0xFFFE, 1, 8000, 16, 9, make([]byte, 24)...))
	f.Fuzz(func(t *testing.T, file []byte) {
		d, measured := wavUploadLength(file)
		if !measured {
			if d != 0 {
				t.Fatalf("unmeasured, yet %v", d)
			}
			return
		}
		clip := &wavClip{}
		clip.write(file)
		if headers := int64(wavRIFFBytes + 2*wavChunkHeaderBytes + wavFmtFieldBytes); clip.received > int64(len(file))-headers {
			t.Fatalf("%d data bytes in a file of %d", clip.received, len(file))
		}
		if want := lengthOf(clip.received, clip.byteRate); d != want {
			t.Fatalf("%v, want %v: %d bytes at %d a second", d, want, clip.received, clip.byteRate)
		}
	})
}

// However hostile a FLAC stream, its length is what its STREAMINFO declares — read
// here as the decoder reads it, one 64-bit word — or it is not measured: never a
// panic, never a length of the longest time.Duration's whole seconds or more.
func FuzzFLACUploadLength(f *testing.F) {
	f.Add(flacStream(44100, 2, 16, 441000)[4:])
	f.Add(flacStream(1, 1, 16, 1<<36-1)[4:])
	f.Add(flacStream(44100, 2, 16, 0)[4:])
	f.Add([]byte{0, 0, 0, 34})
	f.Fuzz(func(t *testing.T, stream []byte) {
		d, measured := flacLength(stream)
		if len(stream) < 38 || stream[0]&0x7F != 0 || binary.BigEndian.Uint32(stream[0:4])&0xFFFFFF != 34 {
			if measured || d != 0 {
				t.Fatalf("no STREAMINFO, yet %v measured %v", d, measured)
			}
			return
		}
		props := binary.BigEndian.Uint64(stream[4+10 : 4+18])
		rate, total := int64(props>>44), int64(props&(1<<36-1))
		if rate == 0 || total == 0 {
			if measured || d != 0 {
				t.Fatalf("rate %d, total %d: %v measured %v", rate, total, d, measured)
			}
			return
		}
		if total/rate >= math.MaxInt64/int64(time.Second) {
			if measured || d != 0 {
				t.Fatalf("%d samples at %d Hz, past the longest length: %v measured %v", total, rate, d, measured)
			}
			return
		}
		if want := lengthOf(total, rate); !measured || d != want {
			t.Fatalf("%d samples at %d Hz: %v measured %v, want %v", total, rate, d, measured, want)
		}
	})
}
