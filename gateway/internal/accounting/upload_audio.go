package accounting

import (
	"bytes"
	"encoding/binary"
	"math"
	"time"
)

// Measuring an upload (docs/specs/GATEWAY.md, Accounting → Media usage: speech-to-text):
// a transcription's file part is held whole in memory, so its audio's length is read
// in one bounded pass — never by the filename or the part's content type. llama-server
// decodes it with miniaudio 0.11.25 (tools/mtmd/mtmd-helper.cpp at b11526,
// ma_decoder_init_memory), which tries its decoders in turn — dr_wav, then dr_flac,
// then dr_mp3 — and takes the first that opens the file; dr_mp3 searches the whole
// file for frames. llama-server processes the length that decoder takes, silence past
// the samples present. So a file is measured only when the decoder that opens it
// provably reads it as the gateway does: each reader below holds a file to the checks
// its decoder makes before taking it, and to a shape whose length the gateway reads
// where that decoder reads it; anything else is unmeasurable — a file a decoder would
// refuse falls through to the next, and to dr_mp3's search. Every size is bounded by
// the bytes present, and a size the bytes do not hold ends the pass as unmeasurable —
// never a loop, a panic or an allocation of a size the file declares.

// The magic bytes llama-server takes a file as audio by, before decoding it.
var (
	flacMarker = []byte("fLaC")
	id3Marker  = []byte("ID3")
)

// id3HeaderBytes is the size of an ID3v2 tag's header, and of its footer.
const id3HeaderBytes = 10

// UploadAudioLength is the exact length of the audio in file, an uploaded audio file,
// and whether it could be measured — as llama-server's decoder opens it:
//   - RIFF, form type WAVE: a WAV file, when dr_wav opens it (wavUploadLength) — one
//     dr_wav refuses is unmeasurable: no other decoder opens a file starting RIFF but
//     dr_mp3's search, which the MP3 reader never follows;
//   - fLaC, an ID3v2 tag or an MPEG audio frame sync: a FLAC stream when dr_flac opens
//     it (flacUploadLength), else an MP3 stream (mp3Length).
//
// Any other file — M4A, Ogg, WebM, text — cannot be measured: llama-server does not
// take it as audio.
func UploadAudioLength(file []byte) (time.Duration, bool) {
	switch {
	case len(file) >= wavRIFFBytes && string(file[0:4]) == "RIFF" && string(file[8:12]) == "WAVE":
		return wavUploadLength(file)
	case bytes.HasPrefix(file, flacMarker) || bytes.HasPrefix(file, id3Marker) || mpegSync(file):
		if d, ok := flacUploadLength(file); ok {
			return d, true
		}
		return mp3Length(file)
	}
	return 0, false
}

// mpegSync reports whether b starts with an MPEG audio frame sync: eleven set bits.
func mpegSync(b []byte) bool {
	return len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0
}

// The limits dr_wav puts on a fmt chunk (MA_DR_WAV_MAX_SAMPLE_RATE, _CHANNELS,
// _BITS_PER_SAMPLE): it refuses a WAV past any of them.
const (
	drWAVMaxSampleRate    = 384000
	drWAVMaxChannels      = 256
	drWAVMaxBitsPerSample = 64
)

// wavUploadLength reads a WAV file as dr_wav opens one from memory (ma_dr_wav_init__internal):
// its chunks from the RIFF header on, each skipped by its size and pad byte — a chunk
// running past the file's end ends the walk with no data chunk, which dr_wav refuses —
// up to the first data chunk; the last fmt chunk before it gives the format
// (readUploadFormat); a fact chunk is at least the 4 bytes dr_wav reads of it. The
// format is read as audio generated is (wavFormat.byteRate), within dr_wav's limits.
// The length is the data chunk's bytes present — its declared size bounded by the file,
// as dr_wav bounds it, so its streamed placeholder 0xFFFFFFFF reads as the rest of the
// file — at the fmt chunk's byte rate: never less than dr_wav's, whole frames of the
// block size.
func wavUploadLength(file []byte) (time.Duration, bool) {
	var format wavFormat
	formatRead := false
	for at := wavRIFFBytes; at+wavChunkHeaderBytes <= len(file); {
		id, size := string(file[at:at+4]), int64(binary.LittleEndian.Uint32(file[at+4:at+8]))
		body := at + wavChunkHeaderBytes
		if id == "data" {
			byteRate, ok := format.uploadByteRate()
			if !formatRead || !ok {
				return 0, false
			}
			return audioDuration(min(size, int64(len(file)-body)), byteRate), true
		}
		end := int64(body) + size + size%2
		if end > int64(len(file)) {
			return 0, false
		}
		switch id {
		case "fmt ":
			f, ok := readUploadFormat(file, body, size)
			if !ok {
				return 0, false
			}
			format, formatRead = f, true
		case "fact":
			if size < 4 {
				return 0, false
			}
		}
		at = int(end)
	}
	return 0, false
}

// readUploadFormat reads the fmt chunk of size bytes whose fields begin at
// file[body:], held within the file with its pad byte, as dr_wav reads it: its 16
// bytes of fields — of a chunk of at least 16 bytes, unlike dr_wav, which reads them
// past a shorter one — then, in a longer chunk, its extension's size: an extensible
// format's extension is the 22 bytes dr_wav reads, its subformat's tag the first two
// bytes of its GUID, in a chunk of at least 40 bytes; any other format's extension
// dr_wav seeks over, so it must lie within the file. A chunk dr_wav seeks past by an
// offset that does not fit in 32 bits is not read: it would seek elsewhere.
func readUploadFormat(file []byte, body int, size int64) (wavFormat, bool) {
	if size < wavFmtFieldBytes || size > math.MaxInt32 {
		return wavFormat{}, false
	}
	f := wavFormat{tag: binary.LittleEndian.Uint16(file[body:]), channels: binary.LittleEndian.Uint16(file[body+2:]),
		rate: binary.LittleEndian.Uint32(file[body+4:]), blockSize: binary.LittleEndian.Uint16(file[body+12:]),
		sampleBits: binary.LittleEndian.Uint16(file[body+14:])}
	if size == wavFmtFieldBytes {
		return f, f.tag != wavFormatExtensible
	}
	// A chunk of 17 bytes holds the extension size's first byte, its pad byte the
	// second: both within the file.
	extension := int(binary.LittleEndian.Uint16(file[body+wavFmtFieldBytes:]))
	if f.tag == wavFormatExtensible {
		if extension != wavExtensionSize || size < wavFmtFieldBytes+wavFmtExtensionBytes {
			return wavFormat{}, false
		}
		f.tag = binary.LittleEndian.Uint16(file[body+wavFmtFieldBytes+8:])
		return f, true
	}
	return f, body+wavFmtFieldBytes+2+extension <= len(file)
}

// uploadByteRate is the byte rate of an uploaded WAV's format (wavFormat.byteRate)
// within dr_wav's limits on its rate, channels and sample size.
func (f wavFormat) uploadByteRate() (int64, bool) {
	if f.rate > drWAVMaxSampleRate || f.channels > drWAVMaxChannels || f.sampleBits > drWAVMaxBitsPerSample {
		return 0, false
	}
	return f.byteRate()
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

// pastID3Tags is b past the ID3v2 tags that start it, every one of them, as dr_flac
// skips them; ok is false when b does not hold one of them whole.
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

// flacUploadLength reads a FLAC stream as dr_flac opens one from memory
// (ma_dr_flac__init_private, native): past the ID3v2 tags that start the file, the fLaC
// marker, then the stream's metadata (flacLength); ok is false when dr_flac would not
// open it — miniaudio then tries dr_mp3.
func flacUploadLength(file []byte) (time.Duration, bool) {
	b, ok := pastID3Tags(file)
	if !ok || !bytes.HasPrefix(b, flacMarker) {
		return 0, false
	}
	return flacLength(b[len(flacMarker):])
}

// The sizes of a FLAC stream's first metadata block: its header, then STREAMINFO's
// fields.
const (
	flacBlockHeaderBytes = 4
	flacStreamInfoBytes  = 34
)

// flacMinBlockBytes are the sizes below which dr_flac refuses a metadata block, by
// type: APPLICATION, VORBIS_COMMENT, CUESHEET and PICTURE; other types have none.
var flacMinBlockBytes = map[byte]int{2: 4, 4: 8, 5: 396, 6: 32}

// flacLength reads a FLAC stream past its fLaC marker: its first metadata block is
// STREAMINFO, 34 bytes, or dr_flac refuses it; the length is its total samples (36
// bits) over its sample rate (20 bits) — the length dr_flac takes, whatever the frames
// that follow hold. dr_flac reads the metadata blocks after it (flacMetadataRead). A
// total of 0 ("unknown") or a rate of 0 cannot be measured, nor can a length of the
// longest time.Duration's whole seconds or more (about 292 years), which a total of
// 2^36 samples at a few hertz declares.
func flacLength(b []byte) (time.Duration, bool) {
	if len(b) < flacBlockHeaderBytes+flacStreamInfoBytes || b[0]&0x7F != 0 ||
		int(b[1])<<16|int(b[2])<<8|int(b[3]) != flacStreamInfoBytes {
		return 0, false
	}
	info := b[flacBlockHeaderBytes : flacBlockHeaderBytes+flacStreamInfoBytes]
	rate := int64(info[10])<<12 | int64(info[11])<<4 | int64(info[12])>>4
	total := int64(info[13]&0x0F)<<32 | int64(binary.BigEndian.Uint32(info[14:18]))
	if rate == 0 || total == 0 || !flacMetadataRead(b[flacBlockHeaderBytes+flacStreamInfoBytes:], b[0]&0x80 != 0) {
		return 0, false
	}
	if total/rate >= math.MaxInt64/int64(time.Second) {
		return 0, false
	}
	// Samples at their rate, as bytes at theirs: the rate is below 2^20, far below
	// maxAudioByteRate.
	return audioDuration(total, rate), true
}

// flacMetadataRead reports whether dr_flac reads the metadata blocks b begins with,
// past STREAMINFO — none when STREAMINFO is the last (last): each block's header, its
// size at least the least dr_flac takes for its type (flacMinBlockBytes), up to the
// block flagged last. Each block must lie within the file: dr_flac reads one running
// past the end as the last, but then fails to seek to the first frame past a seek
// table — a stream cut short in its metadata is not read.
func flacMetadataRead(b []byte, last bool) bool {
	for !last {
		if len(b) < flacBlockHeaderBytes {
			return false
		}
		last = b[0]&0x80 != 0
		size := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		if size < flacMinBlockBytes[b[0]&0x7F] || flacBlockHeaderBytes+size > len(b) {
			return false
		}
		b = b[flacBlockHeaderBytes+size:]
	}
	return true
}
