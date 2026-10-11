package accounting

import (
	"bytes"
	"encoding/binary"
	"time"
)

// An MP3 upload's length is a frame scan, read as llama-server's decoder (miniaudio's
// dr_mp3 and minimp3, miniaudio 0.11.25) reads the stream (docs/specs/GATEWAY.md,
// Accounting → Media usage: speech-to-text):
//   - the ID3v2 tag that starts the file is skipped by its size;
//   - a frame's header gives its length — from its bitrate, sample rate and padding —
//     and its samples, by MPEG version and layer; a frame follows the one before it
//     while its header is of the same stream (version, layer and sample rate);
//   - past bytes that are no such frame — junk, a second tag, ID3v1 or APE tags at the
//     end — the scan searches for the next frame, as the decoder resynchronizes: a
//     valid header whose frame the file holds whole and whose next header is of the
//     same stream, or whose frame ends the file. The decoder asks more of a frame it
//     resynchronizes on (ten headers in a row), so of the frames the scan reads it
//     counts every one the decoder plays — a stream cannot hide frames from it behind
//     junk;
//   - a free-format frame (no bitrate in its header) is no frame to the scan: its
//     length is found only by searching for the next header, a search no bound keeps
//     short. They are rare: no common encoder writes them by default;
//   - the stream's first frame, when a Xing or Info header, is no audio. Its frame
//     count, when it gives one, is the length the decoder takes — silence past the
//     frames present — so the length is the longer of it and the frames counted.
//
// The scan only moves forward: its search tries each position as a frame's start at
// most once, a candidate reading the one header past its frame, and the frames it
// counts never overlap.

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

// valid reports whether h is a frame header the scan reads, as the decoder judges one
// (minimp3's hdr_valid): the sync; MPEG-1 or MPEG-2 of any layer, or MPEG-2.5 of Layer
// III; a layer; a bitrate, free format excluded; a sample rate.
func (h mpegHeader) valid() bool {
	bitrate := h[2] >> 4
	return h[0] == 0xFF && (h[1]&0xF0 == 0xF0 || h[1]&0xFE == 0xE2) && h.layer() != 0 &&
		bitrate != 0 && bitrate != 15 && (h[2]>>2)&3 != 3
}

// sameStream reports whether next, a header, is valid and of h's stream: the same MPEG
// version, layer and sample rate (minimp3's hdr_compare). Its bitrate, padding,
// protection and channels may differ.
func (h mpegHeader) sameStream(next mpegHeader) bool {
	return next.valid() && (h[1]^next[1])&0xFE == 0 && (h[2]^next[2])&0x0C == 0
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

// mp3Scan is what a frame scan of an MP3 stream found.
type mp3Scan struct {
	// frames are the audio frames counted, frameBytes their bytes: frames never
	// overlap, so frameBytes is at most the stream's size.
	frames, frameBytes int64
	// samples are the frames' samples by sample rate (rateSlot): a stream may change
	// rate where the scan resynchronizes.
	samples [len(mpegRates)]int64
	// declared is the length the first frame's Xing or Info header gives: its frame
	// count at that frame's samples and rate; 0 without one.
	declared time.Duration
}

// count counts h's frame.
func (s *mp3Scan) count(h mpegHeader) {
	s.frames++
	s.frameBytes += int64(h.frameBytes())
	s.samples[h.rateSlot()] += h.samples()
}

// length is the stream's length: the frames counted, each at its rate, or the length
// its Xing or Info header declares, when longer.
func (s *mp3Scan) length() time.Duration {
	var d int64
	for slot, n := range s.samples {
		d = SaturatingAdd(d, int64(audioDuration(n, mpegRates[slot])))
	}
	return max(time.Duration(d), s.declared)
}

// mp3Length reads an MP3 stream (scanMP3): it cannot be measured when it holds no
// audio frame — llama-server's decoder then has nothing to play — or when its ID3v2
// tag runs past its end.
func mp3Length(file []byte) (time.Duration, bool) {
	s, ok := scanMP3(file)
	if !ok || s.frames == 0 {
		return 0, false
	}
	return s.length(), true
}

// scanMP3 scans the MP3 stream in file — past the ID3v2 tag that starts it — frame by
// frame; ok is false when that tag runs past the file's end.
func scanMP3(file []byte) (s mp3Scan, ok bool) {
	b := file
	if bytes.HasPrefix(b, id3Marker) {
		n, whole := id3TagBytes(b)
		if !whole {
			return s, false
		}
		b = b[n:]
	}
	var last mpegHeader
	synced, first := false, true
	for at := 0; ; {
		if synced {
			if h, ok := mpegHeaderAt(b, at); ok && last.sameStream(h) && at+h.frameBytes() <= len(b) {
				s.count(h)
				last, at = h, at+h.frameBytes()
				continue
			}
		}
		j := nextMPEGFrame(b, at)
		if j < 0 {
			return s, true
		}
		h, _ := mpegHeaderAt(b, j)
		end := j + h.frameBytes()
		if first {
			first = false
			if frames, isTag := xingFrames(b[j:end], h); isTag {
				s.declared = audioDuration(SaturatingMul(frames, h.samples()), mpegRates[h.rateSlot()])
				last, at, synced = h, end, true
				continue
			}
		}
		s.count(h)
		last, at, synced = h, end, true
	}
}

// nextMPEGFrame is where the next frame starts at or after from — a valid header whose
// frame b holds whole, followed by a header of the same stream or ending within a
// header's size of b's end — or -1 when b holds none. Only a byte 0xFF can start one.
func nextMPEGFrame(b []byte, from int) int {
	for j := from; j+mpegHeaderBytes <= len(b); j++ {
		k := bytes.IndexByte(b[j:len(b)-mpegHeaderBytes+1], 0xFF)
		if k < 0 {
			return -1
		}
		j += k
		h, _ := mpegHeaderAt(b, j)
		if !h.valid() {
			continue
		}
		end := j + h.frameBytes()
		if end > len(b) {
			continue
		}
		if next, ok := mpegHeaderAt(b, end); !ok || h.sameStream(next) {
			return j
		}
	}
	return -1
}

// xingFrames reads frame, a stream's first, as a Xing or Info header — a Layer III
// frame holding "Xing" or "Info" past its side information, as the decoder finds one —
// and the frame count it gives when its frames flag is set, else 0; isTag is false for
// any other frame.
func xingFrames(frame []byte, h mpegHeader) (frames int64, isTag bool) {
	if h.layer() != layerIII {
		return 0, false
	}
	at := mpegHeaderBytes + h.sideInfoBytes()
	if h[1]&0x01 == 0 {
		at += 2 // the CRC
	}
	if at+8 > len(frame) {
		return 0, false
	}
	if tag := string(frame[at : at+4]); tag != "Xing" && tag != "Info" {
		return 0, false
	}
	if frame[at+7]&0x01 == 0 || at+12 > len(frame) {
		return 0, true
	}
	return int64(binary.BigEndian.Uint32(frame[at+8 : at+12])), true
}

// sideInfoBytes is the size of a Layer III frame's side information: 17 bytes mono and
// 32 otherwise on MPEG-1, 9 and 17 on MPEG-2 and 2.5.
func (h mpegHeader) sideInfoBytes() int {
	mono := h[3]>>6 == 3
	switch {
	case h.mpeg1() && mono:
		return 17
	case h.mpeg1():
		return 32
	case mono:
		return 9
	}
	return 17
}
