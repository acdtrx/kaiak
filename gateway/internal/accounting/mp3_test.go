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

// An MP3 upload's length is its frames', each by its version and layer at its rate:
// MPEG-1 and MPEG-2 Layer III, CBR and VBR, MPEG-2.5, Layers I and II, a frame with a
// CRC; past an ID3v2 tag — whose bytes, a frame chain among them, are never read as
// frames — with its footer or not; ID3v1 and APE tags and junk at the end, junk between
// frames, a cut last frame and a second file with its own tag and rate after the
// first; junk after the sync or the tag that makes the file an MP3, which the scan
// passes over. A first frame that is a Xing or Info header is no audio, and the length is
// the longer of the frames present and the count it declares (docs/specs/GATEWAY.md,
// Accounting → Media usage: speech-to-text).
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

	embedded, _ := frames(mpeg1L3, 3)
	tag := id3Tag(append([]byte("GEOB\x00\x00\x05\x00\x00\x00"), embedded...), false)
	footerTag := id3Tag([]byte("TIT2\x00\x00\x00\x06\x00\x00\x03\xff\xfb\x90\x64"), true)
	id3v1 := append([]byte("TAG"), bytes.Repeat([]byte{' '}, 125)...)
	ape := append([]byte("Cover\x00"), 0xFF, 0xD8, 0xFF, 0xE2, 0x0C, 0x58, 0xFF, 0xFB, 0x90, 0x64)
	ape = append(ape, "APETAGEX\xd0\x07\x00\x00\x30\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"...)
	junk := []byte("this is no frame, though it holds a sync \xff\xfb\x90 and stops")
	stereo48k, stereo48kSamples := frames(mp3Frame{version: 1, layer: 3, kbps: 256, rate: 48000}, 40)

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
		{"ID3v1 tag", join(cbr, id3v1), at(cbrSamples, 44100)},
		{"APE then ID3v1 tags", join(tag, cbr, ape, id3v1), at(cbrSamples, 44100)},
		{"junk at the end", join(cbr, junk), at(cbrSamples, 44100)},
		{"junk after a sync", join([]byte{0xFF, 0xE0}, junk, cbr), at(cbrSamples, 44100)},
		{"junk after the tag", join(tag, junk, cbr), at(cbrSamples, 44100)},
		{"junk between frames", join(cbr[:len(cbr)/2+1], junk, cbr), at(cbrSamples+cbrSamples/2, 44100)},
		{"a cut last frame", join(cbr, mpeg1L3.bytes()[:300]), at(cbrSamples, 44100)},
		{"a second file at another rate", join(tag, cbr, id3v1, footerTag, stereo48k),
			at(cbrSamples, 44100) + at(stereo48kSamples, 48000)},
		{"one frame", mpeg1L3.bytes(), at(1152, 44100)},
	} {
		got, measured := UploadAudioLength(c.file)
		if !measured || got != c.want {
			t.Errorf("%s: %v (measured %v), want %v", c.name, got, measured, c.want)
		}
	}
}

// An MP3 stream with no audio frame the scan reads is not measured: a sync followed by
// no frame, an ID3v2 tag running past the end or followed by nothing, a Xing header
// alone, a cut frame, free-format frames, headers of a reserved version, layer, bitrate
// or rate, MPEG-2.5 Layers I and II — which the decoder does not read either.
func TestUnmeasurableMP3Uploads(t *testing.T) {
	header := func(b1, b2 byte) []byte {
		return slices.Repeat(append([]byte{0xFF, b1, b2, 0x00}, make([]byte, 413)...), 5)
	}
	for name, file := range map[string][]byte{
		"a sync and no frame":   append([]byte{0xFF, 0xFB}, []byte("no frame follows these bytes, which hold none")...),
		"a tag past the end":    append(id3Tag(nil, false)[:6], 0x7F, 0x7F, 0x7F, 0x7F),
		"a tag and nothing":     id3Tag([]byte("TIT2\x00\x00\x00\x02\x00\x00\x03K"), false),
		"a Xing header alone":   xingFrame(mpeg1L3, "Xing", 100),
		"a cut frame":           mpeg1L3.bytes()[:400],
		"free format":           header(0xFB, 0x00),
		"reserved version":      header(0xEB, 0x90),
		"reserved layer":        header(0xF9, 0x90),
		"bad bitrate":           header(0xFB, 0xF0),
		"reserved rate":         header(0xFB, 0x9C),
		"MPEG-2.5 Layer I":      header(0xE7, 0x90),
		"MPEG-2.5 Layer II":     header(0xE5, 0x90),
		"the sync check's ID3":  []byte("ID3"),
		"ID3 and a short frame": append(id3Tag(nil, false), mpeg1L3.header()...),
	} {
		if d, measured := UploadAudioLength(file); measured || d != 0 {
			t.Errorf("%s: %v measured %v, want unmeasured", name, d, measured)
		}
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
	mpeg2, _ := frames(mpeg2L3, 3)
	f.Add(mpeg2)
	f.Add([]byte{0xFF, 0xFB, 0x90, 0x64})
	f.Add(append(id3Tag(nil, false), flacStream(8000, 1, 16, 8000)...))
	f.Add(pcmWAV(1, 8000, 16, 64))
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
