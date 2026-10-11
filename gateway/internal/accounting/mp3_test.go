package accounting

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
	"time"
)

// mp3Frame describes an MPEG audio frame the tests write: its MPEG version (1, 2, or
// 25 for 2.5), its layer (1, 2, 3), its bitrate in kbit/s, its sample rate in Hz, and
// whether it is padded, mono, protected by a CRC.
type mp3Frame struct {
	version, layer, kbps, rate int
	padded, mono, crc          bool
}

// The tables of ISO/IEC 11172-3 and 13818-3, kept apart from the reader's: the bitrate
// index of each bitrate by version and layer, the rate index of each rate.
var (
	testBitrates = map[[2]int][]int{
		{1, 1}: {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{1, 2}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{1, 3}: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		{2, 1}: {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{2, 2}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{2, 3}: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
	}
	testRates = map[int][]int{1: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 25: {11025, 12000, 8000}}
)

// header is f's 4-byte header.
func (f mp3Frame) header() []byte {
	versionBits := map[int]byte{1: 3, 2: 2, 25: 0}[f.version]
	table := f.version
	if table == 25 {
		table = 2
	}
	h := []byte{0xFF, 0xE0 | versionBits<<3 | byte(4-f.layer)<<1, 0, 0}
	if !f.crc {
		h[1] |= 1
	}
	h[2] = byte(slices.Index(testBitrates[[2]int{table, f.layer}], f.kbps))<<4 | byte(slices.Index(testRates[f.version], f.rate))<<2
	if f.padded {
		h[2] |= 2
	}
	if f.mono {
		h[3] = 3 << 6
	}
	return h
}

// length is f's length in bytes, as the standards give it: Layer I (12 × bitrate ÷
// rate + padding) × 4; Layer II, and Layer III of MPEG-1, 144 × bitrate ÷ rate +
// padding; Layer III of MPEG-2 and 2.5, 72 × bitrate ÷ rate + padding.
func (f mp3Frame) length() int {
	pad := 0
	if f.padded {
		pad = 1
	}
	bitrate := f.kbps * 1000
	switch {
	case f.layer == 1:
		return (12*bitrate/f.rate + pad) * 4
	case f.layer == 3 && f.version != 1:
		return 72*bitrate/f.rate + pad
	}
	return 144*bitrate/f.rate + pad
}

// samples is how many samples f holds: 384 on Layer I, 1152 on Layer II and on Layer
// III of MPEG-1, 576 on Layer III of MPEG-2 and 2.5.
func (f mp3Frame) samples() int64 {
	switch {
	case f.layer == 1:
		return 384
	case f.layer == 3 && f.version != 1:
		return 576
	}
	return 1152
}

// bytes is f whole: its header, then zeros — no byte of them a frame sync.
func (f mp3Frame) bytes() []byte {
	return append(f.header(), make([]byte, f.length()-4)...)
}

// frames is n frames like f, each padded when CBR at its rate pads it — every 2nd
// one, for these tests — and their length in samples.
func frames(f mp3Frame, n int) ([]byte, int64) {
	var b []byte
	for i := range n {
		g := f
		g.padded = i%2 == 1
		b = append(b, g.bytes()...)
	}
	return b, int64(n) * f.samples()
}

// mp3Frames is n frames like f, as frames writes them.
func mp3Frames(f mp3Frame, n int) []byte {
	b, _ := frames(f, n)
	return b
}

// xingFrame is a Layer III frame like f carrying a Xing or Info header (tag) past its
// side information, which declares frameCount frames when it is not negative.
func xingFrame(f mp3Frame, tag string, frameCount int64) []byte {
	b := f.bytes()
	side := map[[2]bool]int{{true, true}: 17, {true, false}: 32, {false, true}: 9, {false, false}: 17}[[2]bool{f.version == 1, f.mono}]
	at := 4 + side
	if f.crc {
		at += 2
	}
	copy(b[at:], tag)
	if frameCount >= 0 {
		b[at+7] = 0x0F // frames, bytes, TOC and quality
		binary.BigEndian.PutUint32(b[at+8:], uint32(frameCount))
	}
	return b
}

var (
	mpeg1L3  = mp3Frame{version: 1, layer: 3, kbps: 128, rate: 44100}
	mpeg2L3  = mp3Frame{version: 2, layer: 3, kbps: 64, rate: 22050, mono: true}
	mpeg25L3 = mp3Frame{version: 25, layer: 3, kbps: 8, rate: 8000, mono: true}
)

// at is a length of so many samples at rate.
func at(samples int64, rate int) time.Duration { return lengthOf(samples, int64(rate)) }

// An MP3 upload's length is its frames', each by its version and layer at the
// stream's rate: MPEG-1 and MPEG-2 Layer III, CBR and VBR, MPEG-2.5, Layers I and II,
// frames with a CRC, two-channel frames of stereo and joint stereo mixed; past an
// ID3v2 tag — whose bytes, a frame chain among them, are never read as frames — with
// its footer or not; before an ID3v1 tag. A first frame that is a Xing or Info header
// is no audio, and the length is the longer of the frames present and the count it
// declares (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text).
func TestMP3UploadLength(t *testing.T) {
	cbr, cbrSamples := frames(mpeg1L3, 100)
	mpeg2, mpeg2Samples := frames(mpeg2L3, 50)
	mpeg25, mpeg25Samples := frames(mpeg25L3, 9)
	layerI, layerISamples := frames(mp3Frame{version: 1, layer: 1, kbps: 384, rate: 48000}, 20)
	layerII, layerIISamples := frames(mp3Frame{version: 1, layer: 2, kbps: 192, rate: 32000}, 20)
	mpeg2LayerI, mpeg2LayerISamples := frames(mp3Frame{version: 2, layer: 1, kbps: 256, rate: 24000}, 7)
	crc, crcSamples := frames(mp3Frame{version: 1, layer: 3, kbps: 320, rate: 48000, crc: true}, 12)

	var vbr []byte
	for _, kbps := range []int{32, 320, 128, 64, 256, 40, 192, 112} {
		vbr = append(vbr, mp3Frame{version: 1, layer: 3, kbps: kbps, rate: 44100}.bytes()...)
	}
	vbrSamples := int64(8 * 1152)
	var modes []byte
	for i := range 6 {
		frame := mpeg1L3.bytes()
		frame[3] = byte(i%2) << 6 // stereo, then joint stereo
		modes = append(modes, frame...)
	}

	embedded, _ := frames(mpeg1L3, 3)
	tag := id3Tag(append([]byte("GEOB\x00\x00\x05\x00\x00\x00"), embedded...), false)
	footerTag := id3Tag([]byte("TIT2\x00\x00\x00\x06\x00\x00\x03\xff\xfb\x90\x64"), true)

	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	for _, c := range []struct {
		name string
		file []byte
		want time.Duration
	}{
		{"MPEG-1 Layer III CBR", cbr, at(cbrSamples, 44100)},
		{"MPEG-2 Layer III", mpeg2, at(mpeg2Samples, 22050)},
		{"MPEG-2.5 Layer III", mpeg25, at(mpeg25Samples, 8000)},
		{"MPEG-1 Layer I", layerI, at(layerISamples, 48000)},
		{"MPEG-1 Layer II", layerII, at(layerIISamples, 32000)},
		{"MPEG-2 Layer I", mpeg2LayerI, at(mpeg2LayerISamples, 24000)},
		{"with a CRC", crc, at(crcSamples, 48000)},
		{"stereo and joint stereo", modes, at(6*1152, 44100)},
		{"VBR with a Xing header", join(xingFrame(mpeg1L3, "Xing", 8), vbr), at(vbrSamples, 44100)},
		{"VBR without one", vbr, at(vbrSamples, 44100)},
		{"a Xing header with a CRC", join(xingFrame(mp3Frame{version: 1, layer: 3, kbps: 128, rate: 44100, crc: true}, "Xing", 8),
			vbr), at(vbrSamples, 44100)},
		{"MPEG-2 with a Xing header", join(xingFrame(mpeg2L3, "Xing", 50), mpeg2), at(mpeg2Samples, 22050)},
		{"CBR with an Info header and no count", join(xingFrame(mpeg1L3, "Info", -1), cbr), at(cbrSamples, 44100)},
		{"a Xing header declaring more than present", join(xingFrame(mpeg1L3, "Xing", 1000), vbr), at(1000*1152, 44100)},
		{"a Xing header declaring less than present", join(xingFrame(mpeg1L3, "Xing", 2), vbr), at(vbrSamples, 44100)},
		{"ID3v2 tag", join(tag, cbr), at(cbrSamples, 44100)},
		{"ID3v2 tag with a footer", join(footerTag, mpeg2), at(mpeg2Samples, 22050)},
		{"ID3v2, then a Xing header", join(tag, xingFrame(mpeg1L3, "Xing", 8), vbr), at(vbrSamples, 44100)},
		{"ID3v1 tag", join(cbr, id3v1Tag), at(cbrSamples, 44100)},
		{"ID3v2 and ID3v1 tags", join(tag, crc, id3v1Tag), at(crcSamples, 48000)},
		{"one frame", mpeg1L3.bytes(), at(1152, 44100)},
	} {
		got, measured := UploadAudioLength(c.file)
		if !measured || got != c.want {
			t.Errorf("%s: %v (measured %v), want %v", c.name, got, measured, c.want)
		}
	}
}

// id3v1Tag is an ID3v1 tag: TAG, then 125 bytes of fields.
var id3v1Tag = append([]byte("TAG"), bytes.Repeat([]byte{' '}, 125)...)

// An MP3 stream with no audio frame the scan reads is not measured: a sync followed by
// no frame, an ID3v2 tag running past the end or followed by nothing, a Xing header
// alone, a cut frame, free-format frames, headers of a reserved version, layer, bitrate
// or rate, MPEG-2.5 Layers I and II — which the decoder does not read either. Nor is
// a stream other than one unbroken run of frames from the ID3v2 tag to the end, an
// ID3v1 tag aside, each of the first frame's version, layer, rate and channel count:
// llama-server's decoder searches past whatever is no frame of the stream, and plays
// every frame it finds at the first frame's rate — junk before, between or after the
// frames, a second file, an APE tag, a second ID3v2 tag, a cut last frame, the file's
// last 128 bytes starting TAG inside the last frame, a change of version, layer, rate
// or channel count; nor is a Xing or Info header where the decoder could read one other
// than a Layer III stream's first frame, or one whose bytes run past the file's end.
func TestUnmeasurableMP3Uploads(t *testing.T) {
	header := func(b1, b2 byte) []byte {
		return slices.Repeat(append([]byte{0xFF, b1, b2, 0x00}, make([]byte, 413)...), 5)
	}
	cbr := mp3Frames(mpeg1L3, 100)
	tag := id3Tag([]byte("TIT2\x00\x00\x00\x02\x00\x00\x03K"), false)
	ape := append([]byte("Cover\x00"), 0xFF, 0xD8, 0xFF, 0xE2, 0x0C, 0x58, 0xFF, 0xFB, 0x90, 0x64)
	ape = append(ape, "APETAGEX\xd0\x07\x00\x00\x30\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"...)
	junk := []byte("this is no frame, though it holds a sync \xff\xfb\x90 and stops")
	tagInside := bytes.Clone(cbr)
	copy(tagInside[len(tagInside)-id3v1Bytes:], "TAG")
	mono := mpeg1L3
	mono.mono = true
	tiny := mp3Frame{version: 2, layer: 3, kbps: 8, rate: 24000}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	for name, file := range map[string][]byte{
		"a sync and no frame":         append([]byte{0xFF, 0xFB}, []byte("no frame follows these bytes, which hold none")...),
		"a tag past the end":          append(id3Tag(nil, false)[:6], 0x7F, 0x7F, 0x7F, 0x7F),
		"a tag and nothing":           tag,
		"a Xing header alone":         xingFrame(mpeg1L3, "Xing", 100),
		"a cut frame":                 mpeg1L3.bytes()[:400],
		"free format":                 header(0xFB, 0x00),
		"reserved version":            header(0xEB, 0x90),
		"reserved layer":              header(0xF9, 0x90),
		"bad bitrate":                 header(0xFB, 0xF0),
		"reserved rate":               header(0xFB, 0x9C),
		"MPEG-2.5 Layer I":            header(0xE7, 0x90),
		"MPEG-2.5 Layer II":           header(0xE5, 0x90),
		"the sync check's ID3":        []byte("ID3"),
		"ID3 and a short frame":       append(id3Tag(nil, false), mpeg1L3.header()...),
		"junk at the end":             join(cbr, junk),
		"junk after a sync":           join([]byte{0xFF, 0xE0}, junk, cbr),
		"junk after the tag":          join(tag, junk, cbr),
		"junk between frames":         join(cbr[:len(cbr)/2+1], junk, cbr),
		"a cut last frame":            join(cbr, mpeg1L3.bytes()[:300]),
		"a second file":               join(tag, cbr, id3v1Tag, tag, cbr),
		"two ID3v2 tags":              join(tag, tag, cbr),
		"an APE tag":                  join(cbr, ape),
		"APE then ID3v1 tags":         join(cbr, ape, id3v1Tag),
		"TAG inside the last frame":   tagInside,
		"a version change":            join(cbr, mp3Frames(mp3Frame{version: 2, layer: 3, kbps: 64, rate: 22050}, 10)),
		"a layer change":              join(cbr, mp3Frames(mp3Frame{version: 1, layer: 2, kbps: 128, rate: 44100}, 10)),
		"a rate change":               join(cbr, mp3Frames(mp3Frame{version: 1, layer: 3, kbps: 128, rate: 48000}, 10)),
		"a channel change":            join(cbr, mp3Frames(mono, 10)),
		"a Xing header later":         join(cbr[:417], xingFrame(mpeg1L3, "Xing", 100000), cbr),
		"an Info header later":        join(xingFrame(mpeg1L3, "Info", -1), cbr[:417], xingFrame(mpeg1L3, "Info", 5), cbr),
		"a Layer II Xing header":      join(layerIIXing(), mp3Frames(mp3Frame{version: 1, layer: 2, kbps: 192, rate: 44100}, 10)),
		"a Xing header past the file": tiny.bytes(),
	} {
		if d, measured := UploadAudioLength(file); measured || d != 0 {
			t.Errorf("%s: %v measured %v, want unmeasured", name, d, measured)
		}
	}
}

// layerIIXing is a Layer II frame holding Xing where the decoder looks for one — past
// Layer III side information, as it reads any frame — declaring 100 000 frames.
func layerIIXing() []byte {
	f := mp3Frame{version: 1, layer: 2, kbps: 192, rate: 44100}.bytes()
	copy(f[4+32:], "Xing\x00\x00\x00\x01\x00\x01\x86\xa0")
	return f
}

// mp3Repeat is a stream of n frames of the given header, the rest of each zeros: the
// headers the miniaudio decoder was run on for the two tests below.
func mp3Repeat(header []byte, size, n int) []byte {
	return bytes.Repeat(append(bytes.Clone(header), make([]byte, size-len(header))...), n)
}

// A stream whose sample rate changes is not measured: llama-server's decoder plays
// every frame at the first frame's rate. 12 MPEG-2.5 frames at 8 kHz, then 1000
// MPEG-1 frames at 48 kHz — 24.864 s at their own rates — decode in miniaudio 0.11.25
// to 2 316 672 frames at 16 kHz, 144.792 s.
func TestMP3RateChangeIsUnmeasurable(t *testing.T) {
	file := append(mp3Repeat([]byte{0xff, 0xe3, 0x18, 0xc0}, 72, 12), mp3Repeat([]byte{0xff, 0xfb, 0x94, 0xc0}, 384, 1000)...)
	if d, measured := UploadAudioLength(file); measured {
		t.Fatalf("measured %v; miniaudio decodes 144.792 s of it", d)
	}
}

// A stream holding free-format frames is not measured: llama-server's decoder finds
// them by searching for the next header and plays them. 12 frames at 128 kbit/s, then
// 1000 free-format frames of the same size — 288 ms counted if they were skipped —
// decode in miniaudio 0.11.25 to 388 224 frames at 16 kHz, 24.264 s.
func TestMP3FreeFormatFramesAreUnmeasurable(t *testing.T) {
	file := append(mp3Repeat([]byte{0xff, 0xfb, 0x94, 0xc0}, 384, 12), mp3Repeat([]byte{0xff, 0xfb, 0x04, 0xc0}, 384, 1000)...)
	if d, measured := UploadAudioLength(file); measured {
		t.Fatalf("measured %v; miniaudio decodes 24.264 s of it", d)
	}
}

// However hostile an MP3 stream, its scan ends and never panics; the frames it counts
// never overlap and are held whole by the stream, each at least the shortest frame's
// 24 bytes; it is measured exactly when it counted an audio frame; and every upload
// reader — the format chosen by these bytes — ends without a panic too.
func FuzzMP3UploadLength(f *testing.F) {
	cbr, _ := frames(mpeg1L3, 4)
	f.Add(cbr)
	f.Add(append(xingFrame(mpeg1L3, "Xing", 7), cbr...))
	f.Add(append(id3Tag([]byte("TIT2\x00\x00\x00\x02\x00\x00\x03K"), true), cbr...))
	f.Add(append(cbr[:500], cbr...))
	f.Add(append(cbr, id3v1Tag...))
	mpeg2, _ := frames(mpeg2L3, 3)
	f.Add(mpeg2)
	f.Add([]byte{0xFF, 0xFB, 0x90, 0x64})
	f.Add(append(id3Tag(nil, false), flacStream(8000, 1, 16, 8000)...))
	f.Add(pcmWAV(1, 8000, 16, 64))
	f.Add(append(mp3Repeat([]byte{0xff, 0xe3, 0x18, 0xc0}, 72, 2), mp3Repeat([]byte{0xff, 0xfb, 0x94, 0xc0}, 384, 3)...))
	f.Add(append(mp3Repeat([]byte{0xff, 0xfb, 0x94, 0xc0}, 384, 2), mp3Repeat([]byte{0xff, 0xfb, 0x04, 0xc0}, 384, 3)...))
	f.Fuzz(func(t *testing.T, file []byte) {
		UploadAudioLength(file)
		s, ok := scanMP3(file)
		if s.frameBytes > int64(len(file)) || s.frames*24 > s.frameBytes {
			t.Fatalf("%d frames of %d bytes in a stream of %d", s.frames, s.frameBytes, len(file))
		}
		d, measured := mp3Length(file)
		if measured != (ok && s.frames > 0) || d < 0 || (!measured && d != 0) {
			t.Fatalf("%v measured %v, with %d frames counted", d, measured, s.frames)
		}
	})
}
