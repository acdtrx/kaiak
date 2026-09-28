package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// readBlocks reads every block of stream.
func readBlocks(t *testing.T, stream string) ([]Block, error) {
	t.Helper()
	r := NewReader(strings.NewReader(stream), testMaxBlock)
	var blocks []Block
	for {
		b, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return blocks, nil
			}
			return blocks, err
		}
		blocks = append(blocks, b)
	}
}

func TestReaderSplitsBlocksAndKeepsRawBytes(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		data   []string
		noData []bool
	}{
		{name: "LF", stream: "data: {\"a\":1}\n\ndata: [DONE]\n\n", data: []string{`{"a":1}`, "[DONE]"}, noData: []bool{false, false}},
		{name: "CRLF", stream: "data: one\r\n\r\ndata: two\r\n\r\n", data: []string{"one", "two", ""}, noData: []bool{false, false, true}},
		{name: "CR", stream: "data: one\r\rdata: two\r\r", data: []string{"one", "two"}, noData: []bool{false, false}},
		{name: "multi-line data", stream: "data: a\ndata:b\ndata:  c\n\n", data: []string{"a\nb\n c"}, noData: []bool{false}},
		{name: "comment and other fields", stream: ": ping\n\nevent: x\nid: 7\nretry: 5\ndata: d\n\n",
			data: []string{"", "d"}, noData: []bool{true, false}},
		{name: "field without colon", stream: "data\n\n", data: []string{""}, noData: []bool{false}},
		{name: "stray blank line", stream: "\ndata: a\n\n", data: []string{"", "a"}, noData: []bool{true, false}},
		{name: "empty stream", stream: ""},
		{name: "CRLF after CR", stream: "data: a\r\ndata: b\r\n\r\n", data: []string{"a\nb", ""}, noData: []bool{false, true}},
		{name: "fields around data", stream: "event: e\r\ndata:{\"x\":1}\r\nid: 1\r\n\r\n", data: []string{`{"x":1}`, ""}, noData: []bool{false, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks, err := readBlocks(t, c.stream)
			if err != nil {
				t.Fatal(err)
			}
			var raw strings.Builder
			for _, b := range blocks {
				raw.Write(b.Raw)
			}
			if raw.String() != c.stream {
				t.Errorf("raw bytes %q, want %q", raw.String(), c.stream)
			}
			if len(blocks) != len(c.data) {
				t.Fatalf("%d blocks, want %d: %+v", len(blocks), len(c.data), blocks)
			}
			for i, b := range blocks {
				if b.HasData == c.noData[i] || string(b.Data) != c.data[i] {
					t.Errorf("block %d: data %q (has %v), want %q (has %v)", i, b.Data, b.HasData, c.data[i], !c.noData[i])
				}
				// The data spans locate each data line's value in Raw.
				var values []string
				for _, span := range b.DataSpans {
					values = append(values, string(b.Raw[span[0]:span[1]]))
				}
				if got := strings.Join(values, "\n"); got != string(b.Data) || len(b.DataSpans) > 0 != b.HasData {
					t.Errorf("block %d: data spans give %q, want %q", i, got, b.Data)
				}
			}
		})
	}
}

func TestReaderReportsTruncation(t *testing.T) {
	for _, stream := range []string{"data: a\n", "data: a", "data: a\n\ndata: b"} {
		if _, err := readBlocks(t, stream); !errors.Is(err, ErrTruncated) {
			t.Errorf("%q: error %v, want truncation", stream, err)
		}
	}
}

func TestReaderDoesNotWaitPastABlock(t *testing.T) {
	// A block ending in CR is returned without reading ahead for a possible LF: the
	// reader must hand each event over as soon as its blank line is complete.
	pr, pw := io.Pipe()
	defer pr.Close()
	r := NewReader(pr, testMaxBlock)
	go func() {
		_, _ = pw.Write([]byte("data: a\r\r"))
		// Nothing more until the block has been read.
	}()
	b, err := r.Next()
	if err != nil || string(b.Data) != "a" {
		t.Fatalf("got %q %v", b.Data, err)
	}
	pw.Close()
}

func TestReaderBoundsBlockSize(t *testing.T) {
	stream := "data: " + strings.Repeat("x", testMaxBlock) + "\n\n"
	if _, err := readBlocks(t, stream); !errors.Is(err, ErrTooLarge) {
		t.Errorf("error %v, want size limit", err)
	}
}

// testMaxBlock is the block size limit the tests read with.
const testMaxBlock = 1 << 20

func TestReaderNamesEventsAndIDs(t *testing.T) {
	blocks, err := readBlocks(t, "event: config\nid: 7\ndata: {}\n\n: heartbeat\n\nevent:resync\ndata: {}\n\nid\ndata: x\n\nevent: a\nevent: b\nid: 1\nid: 2\ndata: y\n\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		event, id string
		hasID     bool
	}{{"config", "7", true}, {"", "", false}, {"resync", "", false}, {"", "", true}, {"b", "2", true}}
	if len(blocks) != len(want) {
		t.Fatalf("%d blocks, want %d", len(blocks), len(want))
	}
	for i, w := range want {
		if b := blocks[i]; b.Event != w.event || b.ID != w.id || b.HasID != w.hasID {
			t.Errorf("block %d: event %q id %q (has %v), want %q %q (%v)", i, b.Event, b.ID, b.HasID, w.event, w.id, w.hasID)
		}
	}
}
