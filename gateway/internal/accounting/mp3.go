package accounting

import (
	"bytes"
	"encoding/binary"
	"time"
)

// An MP3 upload's length is read as llama-server's decoder (miniaudio's dr_mp3 and its
// minimp3, miniaudio 0.11.25) reads the stream from memory, and only when that reading
// is provably the gateway's (docs/specs/GATEWAY.md, Accounting → Media usage:
// speech-to-text). dr_mp3 searches its whole input for frames and resynchronizes past
// whatever is no frame of the stream — junk, a second file, a frame of another sample
// rate or a free-format one — and plays every frame it finds at the first frame's
// sample rate. So the gateway measures one shape alone: one unbroken run of frames
// from the end of the ID3v2 tag that starts the file (if any) to the end of the
// stream, as dr_mp3 finds that end — before an ID3v1 tag, the file's last 128 bytes
// starting TAG — every frame of the first frame's MPEG version, layer and sample rate
// and of its channel count, none free-format, the last held whole. On that run dr_mp3
// synchronizes on the first frame and plays the frames in order, each once: it plays
// no more samples than the frames hold. Anything else — junk before, between or after
// the frames, a second ID3v2 tag, an APE tag ending the stream, a frame of another
// rate, version, layer or channel count, a free-format frame, a last frame cut short
// — is unmeasurable.
//
// The stream's first frame may be a Xing or Info header, which dr_mp3 finds past the
// frame's header, a CRC and Layer III side information — whatever the frame's layer,
// and whatever its length: the bytes are read from the file, past the frame's end
// when they lie there. On Layer III that frame is no audio, and the frame count it
// declares is the length dr_mp3 takes (less the encoder's delay and padding) — silence
// past the frames present — so the length is the longer of it and the frames counted.
// dr_mp3 reads that header in the first frame that decodes to samples, which a crafted
// stream can make a later one: a Xing or Info header found in any later frame, or in a
// Layer I or II stream's, is unmeasurable.

// mpegHeaderBytes is the size of an MPEG audio frame's header.
const mpegHeaderBytes = 4

// mpegHeader is an MPEG audio frame's header.
type mpegHeader [mpegHeaderBytes]byte

// The layer field's values.
const (
	layerIII = 1
	layerII  = 2
	layerI   = 3
)

// mpegBitrates are the bitrates in kbit/s by MPEG-1 or not, layer (III, II, I) and the
// header's bitrate index — 0 is free format, 15 is not a bitrate.
var mpegBitrates = [2][3][15]int64{
	{ // MPEG-2 and MPEG-2.5
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
	},
	{ // MPEG-1
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
	},
}

// mpegRates are the sample rates in Hz by rate slot (rateSlot): MPEG-1's three, then
// MPEG-2's, then MPEG-2.5's.
var mpegRates = [9]int64{44100, 48000, 32000, 22050, 24000, 16000, 11025, 12000, 8000}

func (h mpegHeader) mpeg1() bool  { return h[1]&0x08 != 0 }
func (h mpegHeader) mpeg25() bool { return h[1]&0x10 == 0 }
func (h mpegHeader) layer() int   { return int(h[1]>>1) & 3 }

// mono reports a single-channel frame: its channel mode is mono. The other modes —
// stereo, joint stereo, dual channel — are two channels to the decoder.
func (h mpegHeader) mono() bool { return h[3]>>6 == 3 }

// valid reports whether h is a frame header the scan reads, as the decoder judges one
// (minimp3's hdr_valid): the sync; MPEG-1 or MPEG-2 of any layer, or MPEG-2.5 of Layer
// III; a layer; a bitrate, free format excluded; a sample rate.
func (h mpegHeader) valid() bool {
	bitrate := h[2] >> 4
	return h[0] == 0xFF && (h[1]&0xF0 == 0xF0 || h[1]&0xFE == 0xE2) && h.layer() != 0 &&
		bitrate != 0 && bitrate != 15 && (h[2]>>2)&3 != 3
}

// sameStream reports whether next, a header, is valid and of h's stream: the same MPEG
// version, layer and sample rate (minimp3's hdr_compare), and the same channel count.
// Its bitrate, padding, protection and mode extension may differ.
func (h mpegHeader) sameStream(next mpegHeader) bool {
	return next.valid() && (h[1]^next[1])&0xFE == 0 && (h[2]^next[2])&0x0C == 0 && h.mono() == next.mono()
}

// rateSlot is h's sample rate's index in mpegRates.
func (h mpegHeader) rateSlot() int {
	version := 2
	switch {
	case h.mpeg1():
		version = 0
	case !h.mpeg25():
		version = 1
	}
	return version*3 + int(h[2]>>2)&3
}

// samples is how many samples h's frame holds per channel: 384 on Layer I, 1152 on
// Layer II, 1152 on Layer III of MPEG-1 and 576 of MPEG-2 and 2.5.
func (h mpegHeader) samples() int64 {
	switch {
	case h.layer() == layerI:
		return 384
	case h.layer() == layerIII && !h.mpeg1():
		return 576
	}
	return 1152
}

// frameBytes is the length of h's frame, its header included: its samples' share of
// its bitrate at its sample rate, whole 4-byte slots on Layer I, then its padding — a
// slot, 4 bytes on Layer I and 1 on the others. Valid headers only: at least 24 bytes.
func (h mpegHeader) frameBytes() int {
	mpeg1 := 0
	if h.mpeg1() {
		mpeg1 = 1
	}
	kbps := mpegBitrates[mpeg1][h.layer()-1][h[2]>>4]
	n := h.samples() * kbps * 125 / mpegRates[h.rateSlot()]
	if h.layer() == layerI {
		n &^= 3
	}
	if h[2]&0x02 != 0 {
		n++
		if h.layer() == layerI {
			n += 3
		}
	}
	return int(n)
}

// mpegHeaderAt is the header at b[at:]; ok is false when b ends before it does.
func mpegHeaderAt(b []byte, at int) (h mpegHeader, ok bool) {
	if at+mpegHeaderBytes > len(b) {
		return h, false
	}
	return mpegHeader(b[at : at+mpegHeaderBytes]), true
}

// The tags dr_mp3 looks for at a stream's end before reading its frames: an ID3v1 tag,
// the file's last 128 bytes when they start TAG; then, before it, an APE tag's footer,
// 32 bytes starting APETAGEX.
const (
	id3v1Bytes     = 128
	apeFooterBytes = 32
)

// mp3Scan is what a scan of an MP3 stream found.
type mp3Scan struct {
	// frames are the audio frames counted, frameBytes their bytes: frames never
	// overlap, so frameBytes is at most the stream's size.
	frames, frameBytes int64
	// samples are the frames' samples, at rate, the stream's sample rate in Hz.
	samples, rate int64
	// declared is the length the first frame's Xing or Info header gives: its frame
	// count at that frame's samples and rate; 0 without one.
	declared time.Duration
}

// count counts h's frame.
func (s *mp3Scan) count(h mpegHeader) {
	s.frames++
	s.frameBytes += int64(h.frameBytes())
	s.samples += h.samples()
}

// length is the stream's length: the frames counted, or the length its Xing or Info
// header declares, when longer.
func (s *mp3Scan) length() time.Duration {
	return max(audioDuration(s.samples, s.rate), s.declared)
}

// mp3Length reads an MP3 stream (scanMP3): it cannot be measured when the scan cannot
// read it, or when it holds no audio frame — llama-server's decoder then has nothing to
// play.
func mp3Length(file []byte) (time.Duration, bool) {
	s, ok := scanMP3(file)
	if !ok || s.frames == 0 {
		return 0, false
	}
	return s.length(), true
}

// scanMP3 scans the MP3 stream in file frame by frame; ok is false when the stream is
// not the one shape the gateway measures — the unbroken run of frames described above.
func scanMP3(file []byte) (s mp3Scan, ok bool) {
	end := len(file)
	if end > id3v1Bytes && string(file[end-id3v1Bytes:end-id3v1Bytes+3]) == "TAG" {
		end -= id3v1Bytes
	}
	if end > apeFooterBytes && string(file[end-apeFooterBytes:end-apeFooterBytes+8]) == "APETAGEX" {
		return s, false
	}
	stream := file[:end]
	start := 0
	if bytes.HasPrefix(stream, id3Marker) {
		n, whole := id3TagBytes(stream)
		if !whole {
			return s, false
		}
		start = n
	}
	first, ok := mpegHeaderAt(stream, start)
	if !ok || !first.valid() {
		return s, false
	}
	s.rate = mpegRates[first.rateSlot()]
	for at := start; at < end; {
		h, ok := mpegHeaderAt(stream, at)
		if !ok || !first.sameStream(h) || at+h.frameBytes() > end {
			return s, false
		}
		tag, frames, readable := xingHeader(file, at, h)
		switch {
		case !readable, tag && (at > start || h.layer() != layerIII):
			return s, false
		case tag && frames > 0:
			s.declared = audioDuration(SaturatingMul(frames, h.samples()), s.rate)
		case !tag:
			s.count(h)
		}
		at += h.frameBytes()
	}
	return s, true
}

// xingHeader reads where dr_mp3 looks for a Xing or Info header in the frame starting
// at file[frame:], of header h: past the header, a CRC when the frame has one, and
// Layer III side information — read from the file, past the frame's end too. tag
// reports one; frames is the frame count it declares when its frames flag is set, else
// 0; readable is false when the bytes dr_mp3 reads lie past the file's end.
func xingHeader(file []byte, frame int, h mpegHeader) (tag bool, frames int64, readable bool) {
	at := frame + mpegHeaderBytes + h.sideInfoBytes()
	if h[1]&0x01 == 0 {
		at += 2 // the CRC
	}
	if at+4 > len(file) {
		return false, 0, false
	}
	if id := string(file[at : at+4]); id != "Xing" && id != "Info" {
		return false, 0, true
	}
	if at+8 > len(file) {
		return true, 0, false
	}
	if file[at+7]&0x01 == 0 {
		return true, 0, true
	}
	if at+12 > len(file) {
		return true, 0, false
	}
	return true, int64(binary.BigEndian.Uint32(file[at+8 : at+12])), true
}

// sideInfoBytes is the size of a Layer III frame's side information, as dr_mp3 reads it
// from any frame it looks for a Xing header in: 17 bytes mono and 32 otherwise on
// MPEG-1, 9 and 17 on MPEG-2 and 2.5.
func (h mpegHeader) sideInfoBytes() int {
	switch {
	case h.mpeg1() && h.mono():
		return 17
	case h.mpeg1():
		return 32
	case h.mono():
		return 9
	}
	return 17
}
