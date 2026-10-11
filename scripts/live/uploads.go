package main

// Uploads: the multipart/form-data bodies of image edits and speech-to-text, and the
// media they carry, written here from code — a tone of known length as WAV and FLAC,
// silence of the same length as MP3, and a small PNG — so the kit ships no binary files
// and knows each file's length exactly.

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/textproto"
)

// formPart is one part of an upload: a text part, or — with a filename — a file part.
type formPart struct {
	name, filename, contentType string
	value                       []byte
}

func textPart(name, value string) formPart { return formPart{name: name, value: []byte(value)} }

func filePart(name, filename, contentType string, data []byte) formPart {
	return formPart{name: name, filename: filename, contentType: contentType, value: data}
}

// formBody writes parts as a multipart/form-data body, as an OpenAI client sends one,
// and returns it with its content type.
func formBody(parts []formPart) ([]byte, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		if p.filename == "" {
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, p.name))
		} else {
			h.Set("Content-Disposition", multipart.FileContentDisposition(p.name, p.filename))
			h.Set("Content-Type", p.contentType)
		}
		// Writing to a bytes.Buffer never fails.
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write(p.value)
	}
	_ = w.Close()
	return b.Bytes(), w.FormDataContentType()
}

// withTextParts adds params as text parts — a value's decimal or string form — and
// returns parts.
func withTextParts(parts []formPart, params map[string]any) []formPart {
	for k, v := range params {
		parts = append(parts, textPart(k, fmt.Sprint(v)))
	}
	return parts
}

// postForm sends parts as an upload to path and reads the whole answer.
func (r *run) postForm(key, path, id string, parts []formPart) (*response, error) {
	body, contentType := formBody(parts)
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.gw.api+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	return r.do(req)
}

// The audio the speech-to-text checks upload, uploadMillis long: the WAV and FLAC a
// 440 Hz tone, 16-bit mono at uploadRate — the rate ASR models take, so no server
// resamples them; the MP3 silence at 32 kHz, MPEG-1's lowest rate, which servers
// resample. 3.6 s is a whole number of MP3 frames (100 of 36 ms) and of samples, and
// rounds up to 4 s with room on both sides: a decoder adding or dropping a frame's
// delay still counts 4.
const (
	uploadRate   = 16000
	uploadMillis = 3600
)

// uploadSeconds is the uploaded audio's length rounded up to a whole second, as the
// gateway counts a record's audio processed.
func uploadSeconds() int64 { return ceilRat(big.NewRat(uploadMillis, 1000)) }

// uploadAudio is a file the speech-to-text checks upload.
type uploadAudio struct {
	format, filename, contentType string
	data                          []byte
}

// uploadAudios are the tone as WAV and FLAC and the silence as MP3 — the formats
// llama-server decodes, and every one vLLM does.
func uploadAudios() []uploadAudio {
	tone := toneSamples()
	return []uploadAudio{
		{"wav", "tone.wav", "audio/wav", wavFile(tone)},
		{"flac", "tone.flac", "audio/flac", flacFile(tone)},
		{"mp3", "silence.mp3", "audio/mpeg", mp3Silence()},
	}
}

// toneSamples is uploadMillis of a 440 Hz tone at uploadRate, about a quarter of full
// scale.
func toneSamples() []int16 {
	samples := make([]int16, uploadRate*uploadMillis/1000)
	for i := range samples {
		samples[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/uploadRate))
	}
	return samples
}

// wavFile is samples as a WAV file: 16-bit PCM, mono, at uploadRate.
func wavFile(samples []int16) []byte {
	le := binary.LittleEndian
	data := 2 * len(samples)
	b := []byte("RIFF")
	b = le.AppendUint32(b, uint32(36+data))
	b = append(b, "WAVEfmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, 1) // PCM
	b = le.AppendUint16(b, 1) // mono
	b = le.AppendUint32(b, uploadRate)
	b = le.AppendUint32(b, 2*uploadRate) // bytes a second
	b = le.AppendUint16(b, 2)            // block size
	b = le.AppendUint16(b, 16)           // bits a sample
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(data))
	for _, s := range samples {
		b = le.AppendUint16(b, uint16(s))
	}
	return b
}

// flacBlock is the FLAC file's samples per frame: every frame but the last holds this
// many.
const flacBlock = 4096

// flacFile is samples as a FLAC file (the format specification, RFC 9639): the
// marker, STREAMINFO — its total samples and the samples' MD5 — then one frame per
// block of flacBlock samples, each a VERBATIM subframe of 16-bit samples, so every
// field falls on a byte and no bit packing is needed. Each frame header carries its
// CRC-8 and each frame its CRC-16, which decoders check. Mono at uploadRate (16 kHz);
// under 128 frames, so a frame number is one byte.
func flacFile(samples []int16) []byte {
	be := binary.BigEndian
	out := []byte("fLaC")
	out = append(out, 0x80, 0, 0, 34) // the last metadata block: STREAMINFO, 34 bytes
	out = be.AppendUint16(out, flacBlock)
	out = be.AppendUint16(out, flacBlock)
	out = append(out, 0, 0, 0, 0, 0, 0) // smallest and largest frame: unknown
	// Sample rate (20 bits), channels − 1 (3: 0, mono), bits a sample − 1 (5: 15),
	// total samples (36).
	out = be.AppendUint64(out, uint64(uploadRate)<<44|15<<36|uint64(len(samples)))
	pcm := make([]byte, 0, 2*len(samples))
	for _, s := range samples {
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(s))
	}
	sum := md5.Sum(pcm)
	out = append(out, sum[:]...)

	for number, start := 0, 0; start < len(samples); number, start = number+1, start+flacBlock {
		n := min(flacBlock, len(samples)-start)
		frame := []byte{0xFF, 0xF8} // sync, fixed block size
		sizeCode := byte(0xC)       // 4096 samples
		if n != flacBlock {
			sizeCode = 0x7 // the size − 1 follows the header, in 16 bits
		}
		frame = append(frame, sizeCode<<4|0x5) // 16 kHz
		frame = append(frame, 0x08)            // mono (channels code 0), 16 bits a sample (code 4)
		frame = append(frame, byte(number))
		if n != flacBlock {
			frame = be.AppendUint16(frame, uint16(n-1))
		}
		frame = append(frame, flacCRC8(frame))
		frame = append(frame, 0x02) // a VERBATIM subframe, no wasted bits
		for _, s := range samples[start : start+n] {
			frame = be.AppendUint16(frame, uint16(s))
		}
		frame = be.AppendUint16(frame, flacCRC16(frame))
		out = append(out, frame...)
	}
	return out
}

// flacCRC8 is FLAC's frame header check: polynomial x⁸ + x² + x + 1, from 0.
func flacCRC8(data []byte) byte {
	var crc byte
	for _, b := range data {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// flacCRC16 is FLAC's frame check: polynomial x¹⁶ + x¹⁵ + x² + 1, from 0.
func flacCRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// The MP3 file: mp3Frames frames of MPEG-1 Layer III, 32 kHz, 32 kbps, mono, no CRC —
// 144 bytes each, no padding needed (144 × 32 000 ÷ 32 000), 1152 samples (36 ms)
// each.
const (
	mp3Frames    = uploadMillis / 36
	mp3FrameSize = 144
)

// mp3Silence is uploadMillis of silent MP3: every frame its header, then zeros — side
// information of zero (no main data, no scale factors) and a body of zeros, which
// every decoder plays as silence, as encoders write digital silence. No encoder is
// needed, and the length is exact: the frames' samples. The file is one unbroken run
// of identical frames and nothing else — no tags, no trailing bytes — which the
// gateway measures for llama-server, taking an MP3 only when its decoder provably
// reads it as measured (docs/specs/GATEWAY.md, Accounting → Media usage:
// speech-to-text).
func mp3Silence() []byte {
	frame := make([]byte, mp3FrameSize)
	// Sync, MPEG-1, Layer III, no CRC; 32 kbps, 32 kHz, no padding; mono.
	copy(frame, []byte{0xFF, 0xFB, 0x18, 0xC0})
	return bytes.Repeat(frame, mp3Frames)
}

// editImage is the input image of the edit checks: a 512×512 PNG of a lighthouse — a
// grey tower on a dark sea under a blue sky — drawn here.
func editImage() []byte {
	const size = 512
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			c := color.RGBA{R: 90, G: 150, B: 220, A: 255} // sky
			switch {
			case y > 380:
				c = color.RGBA{R: 20, G: 50, B: 90, A: 255} // sea
			case x > 230 && x < 282 && y > 150:
				c = color.RGBA{R: 200, G: 200, B: 200, A: 255} // tower
			case x > 220 && x < 292 && y > 120 && y <= 150:
				c = color.RGBA{R: 250, G: 220, B: 90, A: 255} // lamp room
			}
			img.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	// Encoding an in-memory image to a bytes.Buffer never fails.
	_ = png.Encode(&b, img)
	return b.Bytes()
}
