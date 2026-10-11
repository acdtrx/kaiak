package accounting

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// imageElement is one data element as vLLM-Omni writes it: the image's bytes in
// standard base64.
func imageElement(size int) string {
	image := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, max(size-8, 0))...)
	return `{"b64_json":"` + base64.StdEncoding.EncodeToString(image) + `","url":null,"revised_prompt":null}`
}

// imagesBody is an image answer holding images of size bytes each, its members in
// vLLM-Omni's order.
func imagesBody(images, size int) []byte {
	elements := make([]string, images)
	for i := range elements {
		elements[i] = imageElement(size)
	}
	return []byte(`{"created":1700000000,"data":[` + strings.Join(elements, ",") +
		`],"output_format":"png","size":"1024x1024","cot_output":null,"metrics":{}}`)
}

// meterImages runs an image answer through a meter for a request asking for n images
// — answered 200, its body in pieces of piece bytes — and settles it.
func meterImages(n int64, body []byte, piece int, complete bool) (Units, Flags) {
	m := NewMeter(provider.ImagesGenerations, 33, MediaRequest{Images: n})
	m.Answered(http.StatusOK, false, http.Header{"Content-Type": {"application/json"}})
	for len(body) > 0 {
		k := min(piece, len(body))
		m.Observe(provider.Event{Data: body[:k]})
		body = body[k:]
	}
	return m.Settle(complete)
}

// imageUnits is an image record's units: its estimated input and its images.
func imageUnits(images int64) Units {
	return withEveryUnit(Units{config.UnitTokensIn: 33, config.UnitImages: images})
}

// An image answer's images are the elements of its top-level data, counted as the body
// passes in pieces of any size — the answer's count, which a layered model makes
// larger than the n asked for — read from where the answer puts them: data nested in
// another member, or written in a string, is not the answer's; a name escaped is the
// same name; the data named last is read; elements of any kind count. The tokens are
// always estimated — the input estimate, output 0 — a usage the answer carries
// unread (docs/specs/GATEWAY.md, Accounting → Media usage).
func TestImagesCountTheAnswersData(t *testing.T) {
	const bs = `\` // a JSON escape begins with it
	for _, c := range []struct {
		name   string
		n      int64
		body   string
		images int64
	}{
		{"one image", 1, string(imagesBody(1, 100)), 1},
		{"four images", 4, string(imagesBody(4, 100)), 4},
		{"a layered answer, past n", 1, string(imagesBody(5, 100)), 5},
		{"fewer than n", 3, string(imagesBody(2, 100)), 2},
		{"no data", 1, `{"created":1,"output_format":"png"}`, 0},
		{"data empty", 2, `{"created":1,"data":[]}`, 0},
		{"data null", 2, `{"created":1,"data":null}`, 0},
		{"data not a list", 2, `{"created":1,"data":{"b64_json":"iVBORw=="}}`, 0},
		{"elements of any kind", 1, `{"data":[1,"x",null,{},[]]}`, 5},
		{"data nested elsewhere", 1, `{"metrics":{"data":[{},{},{}]},"cot_output":["data",[{}]],"data":[{"b64_json":"iVBORw=="}]}`, 1},
		{"data in a string", 1, `{"revised":"\"data\":[{},{},{}]","data":[{"b64_json":"iVBO\"RW]=="}]}`, 1},
		{"an escaped name", 1, `{"` + bs + `u0064ata":[{},{}]}`, 2},
		{"a name escaped otherwise", 1, `{"dat\"a":[{},{}],"data":[{}]}`, 1},
		{"data named twice: the last", 1, `{"data":[{},{},{}],"output_format":"png","data":[{}]}`, 1},
		{"brackets and braces in strings", 1, `{"data":[{"b64_json":"[{,}]"},{"revised_prompt":"]},{"}]}`, 2},
		{"indented", 2, "{\n  \"created\": 1,\n  \"data\": [\n    {\"b64_json\": \"iVBORw==\"} ,\n    {}\n  ]\n}\n", 2},
		{"a usage, unread", 1, `{"data":[{}],"usage":{"input_tokens":900,"output_tokens":4000,"total_tokens":4900}}`, 1},
		{"a long name", 1, `{"` + strings.Repeat("d", 200) + `":[{},{}],"data":[{}]}`, 1},
		{"not an object", 2, `[{"data":[{}]}]`, 0},
	} {
		for _, piece := range pieceSizes {
			units, flags := meterImages(c.n, []byte(c.body), piece, true)
			if want := imageUnits(c.images); !maps.Equal(units, want) || !flags.Estimated || flags.Partial {
				t.Errorf("%s, in pieces of %d: %v %+v, want %v, estimated", c.name, piece, units, flags, want)
			}
		}
	}
}

// The images counted in a valid answer are the elements a JSON decoder finds in its
// top-level data — the last one named, by its exact name after decoding — whatever
// they hold, however the answer is split (docs/specs/GATEWAY.md, Accounting → Media
// usage).
func FuzzImagesCountIsTheDecodersCount(f *testing.F) {
	f.Add([]byte(`{"data":[{},{}]}`))
	f.Add([]byte(`{"data":[{},{}],"data":[{"b64_json":"abc\\def\"ghi"}],"nested":{"data":[1,2,3]}}`))
	f.Add([]byte(`{"\u0064ata":[null,[1,2],{"data":[]}],"Data":[{}]}`))
	f.Add([]byte(`{"data":null}`))
	f.Add(imagesBody(3, 64))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 64<<10 {
			t.Skip()
		}
		var answer map[string]json.RawMessage
		if json.Unmarshal(body, &answer) != nil || answer == nil {
			return
		}
		var data []json.RawMessage
		_ = json.Unmarshal(answer["data"], &data) // anything but a list holds none
		for _, piece := range []int{1, 7, 32 << 10} {
			if units, _ := meterImages(1, body, piece, true); units[config.UnitImages] != int64(len(data)) {
				t.Fatalf("pieces of %d: %d images, the decoder finds %d", piece, units[config.UnitImages], len(data))
			}
		}
	})
}

// An image answer cut short counts the larger of the elements seen and the images
// asked for — they were generated before the answer's first byte — and so does an
// event stream answering an image request, which is no image answer; flagged partial.
// An error answer counts nothing; a request sent and never answered its input alone
// (docs/specs/GATEWAY.md, Accounting → Partial, Billed from the moment the request
// was sent).
func TestImagesCutShortAndUnanswered(t *testing.T) {
	whole := string(imagesBody(4, 300))
	firstElement := strings.Index(whole, `{"b64_json"`)
	thirdElement := firstElement + 2*(len(imageElement(300))+1)
	for _, c := range []struct {
		name   string
		n      int64
		body   string
		images int64
	}{
		{"within the first image", 3, whole[:firstElement+100], 3},
		{"within the third image, n 2", 2, whole[:thirdElement+100], 3},
		{"within the third image, n 4", 4, whole[:thirdElement+100], 4},
		{"before data", 2, whole[:firstElement-4], 2},
		{"after data, a layered answer", 1, whole[:len(whole)-10], 4},
		{"nothing relayed", 2, "", 2},
	} {
		for _, piece := range pieceSizes {
			units, flags := meterImages(c.n, []byte(c.body), piece, false)
			if want := imageUnits(c.images); !maps.Equal(units, want) || !flags.Estimated || !flags.Partial {
				t.Errorf("%s, in pieces of %d: %v %+v, want %v, estimated and partial", c.name, piece, units, flags, want)
			}
		}
	}

	m := NewMeter(provider.ImagesGenerations, 33, MediaRequest{Images: 3})
	m.Answered(http.StatusOK, true, http.Header{"Content-Type": {"text/event-stream"}})
	m.Observe(provider.Event{Data: []byte("data: " + whole + "\n\n"), Payload: []byte(whole)})
	if units, flags := m.Settle(false); !maps.Equal(units, imageUnits(3)) || !flags.Estimated || !flags.Partial {
		t.Errorf("an event stream: %v %+v, want the 3 images asked for, estimated and partial", units, flags)
	}

	m = NewMeter(provider.ImagesGenerations, 33, MediaRequest{Images: 3})
	m.Answered(http.StatusBadRequest, false, http.Header{"Content-Type": {"application/json"}})
	m.Observe(provider.Event{Data: []byte(`{"error":{"message":"n: Input should be less than or equal to 10"}}`)})
	if units, _ := m.Settle(true); !maps.Equal(units, withEveryUnit(Units{})) {
		t.Errorf("an error answer: %v, want zeros", units)
	}

	m = NewMeter(provider.ImagesGenerations, 33, MediaRequest{Images: 3})
	m.Sent()
	if units, flags := m.Settle(false); !maps.Equal(units, withEveryUnit(Units{config.UnitTokensIn: 33})) ||
		!flags.Estimated || !flags.Partial {
		t.Errorf("sent, never answered: %v %+v, want the input alone, estimated and partial", units, flags)
	}
}

// An image edit is answered as a generation is, and metered the same way: the answer's
// data counted — a layered answer past n — the tokens the input estimate, flagged
// estimated; cut short, at least the images asked for, flagged partial
// (docs/specs/GATEWAY.md, Accounting → Media usage: image edits; Partial).
func TestImageEditsCountAsGenerations(t *testing.T) {
	whole := imagesBody(3, 100)
	firstElement := strings.Index(string(whole), `{"b64_json"`)
	for _, c := range []struct {
		name     string
		body     []byte
		complete bool
		images   int64
	}{
		{"a layered answer", whole, true, 3},
		{"cut within the first image", whole[:firstElement+50], false, 2},
	} {
		m := NewMeter(provider.ImagesEdits, 33, MediaRequest{Images: 2})
		m.Answered(http.StatusOK, false, http.Header{"Content-Type": {"application/json"}})
		m.Observe(provider.Event{Data: c.body})
		units, flags := m.Settle(c.complete)
		if !maps.Equal(units, imageUnits(c.images)) || !flags.Estimated || flags.Partial == c.complete {
			t.Errorf("%s: %v %+v, want %v, estimated, partial %v", c.name, units, flags, imageUnits(c.images), !c.complete)
		}
	}
}

// An image answer of many MiB is counted in one pass, nothing of it held: metering
// four images of 3 MiB each (a body of 16 MiB) allocates about what metering four tiny
// ones does (docs/specs/GATEWAY.md, Accounting → Media usage: never kept).
func TestImagesCountWithoutHolding(t *testing.T) {
	small, large := imagesBody(4, 100), imagesBody(4, 3<<20)
	if units, _ := meterImages(1, large, 32<<10, true); units[config.UnitImages] != 4 {
		t.Fatalf("large answer: %v, want 4 images", units)
	}
	allocated := func(body []byte) uint64 {
		const runs = 3
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for range runs {
			meterImages(1, body, 32<<10, true)
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}
	const margin = 256 << 10
	if few, many := allocated(small), allocated(large); many > few+margin {
		t.Errorf("metering allocates %d bytes for an answer of %d, %d for one of %d: it grows with the answer",
			few, len(small), many, len(large))
	}
}
