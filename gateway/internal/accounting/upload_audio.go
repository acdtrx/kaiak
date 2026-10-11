package accounting

import (
	"bytes"
	"encoding/binary"
	"math"
	"time"
)

// Measuring an upload (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text):
// a transcription's file part is held whole in memory, so its audio's length is read
// in one bounded pass, the format chosen by its magic bytes as llama-server's decoder
// chooses it — never by the filename or the part's content type. llama-server decodes
// WAV, FLAC and MP3 alone, with miniaudio (tools/mtmd/mtmd-helper.cpp at b11526,
// miniaudio 0.11.25): it takes the audio's length from the decoder and processes that
// many samples, silence past the samples present, so each reader reads the length
// where that decoder does. Every size is bounded by the bytes present, and a size the
// bytes do not hold ends the pass as unmeasurable — never a loop, a panic or an
// allocation of a size the file declares.

// The magic bytes llama-server tells its audio formats by.
var (
	flacMarker = []byte("fLaC")
	id3Marker  = []byte("ID3")
)

// id3HeaderBytes is the size of an ID3v2 tag's header, and of its footer.
const id3HeaderBytes = 10

// UploadAudioLength is the exact length of the audio in file, an uploaded audio file,
// and whether it could be measured — chosen by its magic bytes, as llama-server's
// decoder chooses:
//   - RIFF, form type WAVE: a WAV file (wavUploadLength);
//   - fLaC: a FLAC stream (flacLength);
//   - an ID3v2 tag, or an MPEG audio frame sync: a FLAC stream when fLaC follows the
//     leading ID3v2 tags, which the FLAC decoder skips and tries before the MP3 one;
//     else an MP3 stream (mp3Length).
//
// Any other file — M4A, Ogg, WebM, text — cannot be measured.
func UploadAudioLength(file []byte) (time.Duration, bool) {
	switch {
	case len(file) >= wavRIFFBytes && string(file[0:4]) == "RIFF" && string(file[8:12]) == "WAVE":
		return wavUploadLength(file)
	case bytes.HasPrefix(file, flacMarker):
		return flacLength(file[len(flacMarker):])
	case bytes.HasPrefix(file, id3Marker) || mpegSync(file):
		if rest, ok := pastID3Tags(file); ok && bytes.HasPrefix(rest, flacMarker) {
			return flacLength(rest[len(flacMarker):])
		}
		return mp3Length(file)
	}
	return 0, false
}

// mpegSync reports whether b starts with an MPEG audio frame sync: eleven set bits.
func mpegSync(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0
}

// wavUploadLength reads a WAV file as audio generated is read (wavClip), the whole file
// at once: rate, channels and block size from the fmt chunk — PCM, IEEE float, A-law,
// μ-law, or the extensible format naming one — and the data chunk's bytes present, at
// most the size it declares. That is the length llama-server's decoder takes: the data
// chunk's size bounded by the file, its placeholder 0xFFFFFFFF read as the rest of the
// file, over the block size.
func wavUploadLength(file []byte) (time.Duration, bool) {
	clip := &wavClip{}
	clip.write(file)
	return clip.length()
}

// id3TagBytes is the size of the ID3v2 tag that starts b — its header, the size the
// header declares in four bytes of 7 bits each, and its footer when its flags say one
// follows — read as llama-server's decoders read it, from each size byte's low 7 bits;
// ok is false when b does not hold the whole tag.
func id3TagBytes(b []byte) (n int, ok bool) {
	if len(b) < id3HeaderBytes {
		return 0, false
	}
	n = int(b[6]&0x7F)<<21 | int(b[7]&0x7F)<<14 | int(b[8]&0x7F)<<7 | int(b[9]&0x7F)
	n += id3HeaderBytes
	if b[5]&0x10 != 0 {
		n += id3HeaderBytes
	}
	if n > len(b) {
		return 0, false
	}
	return n, true
}

// pastID3Tags is b past the ID3v2 tags that start it, every one of them, as the FLAC
// decoder skips them; ok is false when b does not hold one of them whole.
func pastID3Tags(b []byte) ([]byte, bool) {
	for bytes.HasPrefix(b, id3Marker) {
		n, ok := id3TagBytes(b)
		if !ok {
			return nil, false
		}
		b = b[n:]
	}
	return b, true
}

// The sizes of a FLAC stream's first metadata block: its header, then STREAMINFO's
// fields.
const (
	flacBlockHeaderBytes = 4
	flacStreamInfoBytes  = 34
)

// flacLength reads a FLAC stream past its fLaC marker: its first metadata block is
// STREAMINFO, 34 bytes, or the stream cannot be measured; the length is its total
// samples (36 bits) over its sample rate (20 bits) — the length llama-server's decoder
// takes, whatever the frames that follow hold. A total of 0 ("unknown") or a rate of 0
// cannot be measured, nor can a length of the longest time.Duration's whole seconds or
// more (about 292 years), which a total of 2^36 samples at a few hertz declares.
func flacLength(b []byte) (time.Duration, bool) {
	if len(b) < flacBlockHeaderBytes+flacStreamInfoBytes || b[0]&0x7F != 0 ||
		int(b[1])<<16|int(b[2])<<8|int(b[3]) != flacStreamInfoBytes {
		return 0, false
	}
	info := b[flacBlockHeaderBytes : flacBlockHeaderBytes+flacStreamInfoBytes]
	rate := int64(info[10])<<12 | int64(info[11])<<4 | int64(info[12])>>4
	total := int64(info[13]&0x0F)<<32 | int64(binary.BigEndian.Uint32(info[14:18]))
	if rate == 0 || total == 0 {
		return 0, false
	}
	if total/rate >= math.MaxInt64/int64(time.Second) {
		return 0, false
	}
	// Samples at their rate, as bytes at theirs: the rate is below 2^20, far below
	// maxAudioByteRate.
	return audioDuration(total, rate), true
}
