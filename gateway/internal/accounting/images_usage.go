package accounting

import (
	"encoding/json"
	"net/http"

	"kaiak/internal/config"
)

// imagesUsage reads an image generation answer (docs/specs/GATEWAY.md, Accounting →
// Media usage): a JSON body, {"created", "data": [{"b64_json", "url",
// "revised_prompt"}, …], "output_format", "size", …}, whose top-level data elements
// are the images. They are counted in one pass as the body streams through, never
// kept: up to ten base64 images of several MiB each. The count is the answer's — a
// layered model answers one image per layer, more than the request's n — and the data
// an answer names last is the one read, as a client reads it. vLLM-Omni reports no
// usage for images: the tokens are always estimated — the input estimate, output 0 —
// and a usage the answer carries is not read. An answer cut short counts the larger
// of the elements seen and the images asked for: its images were generated before its
// first byte. Members are read from where the answer puts them — the top-level data,
// its elements — never searched for by what they look like.
type imagesUsage struct {
	// requested are the images the request asked for (MediaRequest.Images).
	requested int64
	// images are the elements of the data list being read, or last read.
	images int64

	phase imagesPhase
	// key holds the member name being read, up to maxScanKey bytes; escaped: its last
	// byte began an escape. data: the member whose value comes next is data.
	key     []byte
	escaped bool
	data    bool
	// skip follows the value being skipped, and resume is the phase after it.
	skip   valueSkip
	resume imagesPhase
}

// imagesPhase is the part of an image answer the reader is in.
type imagesPhase int

const (
	// imagesStart: before the answer's object.
	imagesStart imagesPhase = iota
	// imagesKey: in the answer's object, a member's name or the object's end.
	imagesKey
	// imagesKeyString: inside a member's name.
	imagesKeyString
	// imagesColon: after a member's name.
	imagesColon
	// imagesValue: a member's value.
	imagesValue
	// imagesAfterValue: after a member's value — the next member or the object's end.
	imagesAfterValue
	// imagesElements: the data list — an element or the list's end.
	imagesElements
	// imagesAfterElement: after an element — the next or the list's end.
	imagesAfterElement
	// imagesSkip: inside a value skipped, then resume.
	imagesSkip
	// imagesDone: the answer's object ended, or the body is not JSON the reader can
	// follow: nothing more is read.
	imagesDone
)

func (u *imagesUsage) streamEvent([]byte) {}

// contentMember: none — the answer's content is images.
func (u *imagesUsage) contentMember() string { return "" }

// bodyUsage: a usage an image answer carries is not read; images are what the
// endpoint counts.
func (u *imagesUsage) bodyUsage(json.RawMessage) {}

func (u *imagesUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *imagesUsage) streamContentBytes() int64 { return 0 }

// reported: never — vLLM-Omni reports no usage for images.
func (u *imagesUsage) reported() (Units, bool) { return nil, true }

// answered: the answer's headers say nothing the reader reads. An event stream
// answering an image request is no image answer: its events are not read, and it never
// completes.
func (u *imagesUsage) answered(http.Header, bool) {}

// mediaSeen: never — an image answer is a body, never timed to its first image.
func (u *imagesUsage) mediaSeen() bool { return false }

// media: the answer's images — once cut short, at least the images asked for.
func (u *imagesUsage) media(complete bool) (Units, bool) {
	images := u.images
	if !complete {
		images = max(images, u.requested)
	}
	return Units{config.UnitImages: images}, true
}

// bodyPiece reads the answer's next bytes, as relayed.
func (u *imagesUsage) bodyPiece(p []byte) {
	for len(p) > 0 && u.phase != imagesDone {
		if u.phase == imagesSkip {
			var ended bool
			if p, ended = u.skip.run(p); ended {
				u.phase = u.resume
			}
			continue
		}
		if u.step(p[0]) {
			p = p[1:]
		}
	}
}

// step reads one byte of the answer's structure and reports whether it took it: the
// first byte of a value skipped is the skip's.
func (u *imagesUsage) step(c byte) bool {
	switch u.phase {
	case imagesStart:
		switch {
		case isJSONSpace(c):
		case c == '{':
			u.phase = imagesKey
		default:
			u.phase = imagesDone
		}
	case imagesKey:
		switch {
		case isJSONSpace(c):
		case c == '"':
			u.phase, u.key, u.escaped = imagesKeyString, u.key[:0], false
		default:
			// The object's end, or a body the reader cannot follow.
			u.phase = imagesDone
		}
	case imagesKeyString:
		switch {
		case u.escaped:
			u.escaped = false
		case c == '\\':
			u.escaped = true
		case c == '"':
			name, ok := scannedName(u.key)
			u.phase, u.data = imagesColon, ok && name == "data"
			return true
		}
		if len(u.key) <= maxScanKey {
			u.key = append(u.key, c)
		}
	case imagesColon:
		switch {
		case isJSONSpace(c):
		case c == ':':
			u.phase = imagesValue
		default:
			u.phase = imagesDone
		}
	case imagesValue:
		return u.value(c)
	case imagesAfterValue:
		switch {
		case isJSONSpace(c):
		case c == ',':
			u.phase = imagesKey
		default:
			u.phase = imagesDone
		}
	case imagesElements:
		switch {
		case isJSONSpace(c):
		case c == ']':
			u.phase = imagesAfterValue
		case c == ',' || c == '}' || c == ':':
			u.phase = imagesDone
		default:
			// An image, whatever the element holds: the answer's count is its
			// elements'.
			u.images++
			u.startSkip(c, imagesAfterElement)
			return false
		}
	case imagesAfterElement:
		switch {
		case isJSONSpace(c):
		case c == ',':
			u.phase = imagesElements
		case c == ']':
			u.phase = imagesAfterValue
		default:
			u.phase = imagesDone
		}
	}
	return true
}

// value reads the first byte of a member's value: the data list is walked, anything
// else skipped.
func (u *imagesUsage) value(c byte) bool {
	switch {
	case isJSONSpace(c):
		return true
	case c == ',' || c == '}' || c == ']' || c == ':':
		u.phase = imagesDone
		return true
	}
	if u.data {
		// A client reads the last data an answer names; so does the reader.
		u.images = 0
		if c == '[' {
			u.phase = imagesElements
			return true
		}
	}
	u.startSkip(c, imagesAfterValue)
	return false
}

// startSkip skips the value whose first byte is c, then reads on in resume.
func (u *imagesUsage) startSkip(c byte, resume imagesPhase) {
	u.skip.start(c, nil)
	u.resume, u.phase = resume, imagesSkip
}
