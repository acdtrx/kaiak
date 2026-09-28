// Package sse reads server-sent event streams (the WHATWG text/event-stream format)
// block by block. The provider reads backend streams with it and relays each block's
// raw bytes; the control client follows the control plane's config stream with it.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// Block is one block of a server-sent event stream: the lines up to and including
// the blank line that ends them.
type Block struct {
	// Raw is the block's bytes, line terminators included. Forwarding Raw gives the
	// reader's client exactly the bytes the server sent.
	Raw []byte
	// Data is the event's data: its data lines joined by "\n". HasData is false for a
	// block with no data line (comments, keep-alives, stray blank lines), which
	// dispatches no event.
	Data    []byte
	HasData bool
	// DataSpans are the byte ranges [start, end) in Raw of each data line's value, in
	// order: Data is these values joined by "\n".
	DataSpans [][2]int
	// Event is the value of the block's last "event" line, "" when it has none (the
	// format then calls the event "message").
	Event string
	// ID is the value of the block's last "id" line; HasID reports whether it had one.
	ID    string
	HasID bool
}

// Reader splits a text/event-stream body into blocks: lines end in CRLF, LF or CR; a
// blank line ends a block; a line starting with ":" is a comment; "data" lines
// accumulate the event's data, one leading space after the colon removed; "event" and
// "id" lines name the event and its ID. Other fields stay in Raw untouched.
type Reader struct {
	r        *bufio.Reader
	maxBlock int
	// skipLF: the previous line ended in CR, so an LF that comes next belongs to it.
	// It is resolved when the next line is read, never by waiting for more input once
	// a block has ended.
	skipLF bool
}

// NewReader returns a reader of r whose blocks are at most maxBlockBytes long, so a
// server that never ends an event cannot make the reader buffer without limit.
func NewReader(r io.Reader, maxBlockBytes int) *Reader {
	return &Reader{r: bufio.NewReader(r), maxBlock: maxBlockBytes}
}

var (
	// ErrTruncated: the stream ended inside a block.
	ErrTruncated = errors.New("event stream ended inside an event")
	// ErrTooLarge: a block passed the reader's size limit.
	ErrTooLarge = errors.New("event stream block exceeds the size limit")
)

// Next returns the next block; io.EOF when the stream ends between blocks.
func (s *Reader) Next() (Block, error) {
	var block Block
	var data bytes.Buffer
	started := false
	for {
		raw, content, terminated, err := s.readLine(s.maxBlock - len(block.Raw))
		lineStart := len(block.Raw)
		block.Raw = append(block.Raw, raw...)
		if err != nil {
			return block, err
		}
		if !terminated {
			// End of stream.
			switch {
			case started || len(content) > 0:
				return block, ErrTruncated
			case len(block.Raw) > 0:
				// Only the LF completing the previous block's CRLF was left.
				return block, nil
			default:
				return block, io.EOF
			}
		}
		if len(content) == 0 {
			if block.HasData {
				block.Data = data.Bytes()
			}
			return block, nil
		}
		started = true
		field, value := splitField(content)
		switch string(field) {
		case "data":
			if block.HasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			block.HasData = true
			// value is a suffix of the line's content, which the one-byte
			// terminator follows.
			end := lineStart + len(raw) - 1
			block.DataSpans = append(block.DataSpans, [2]int{end - len(value), end})
		case "event":
			block.Event = string(value)
		case "id":
			block.ID, block.HasID = string(value), true
		}
	}
}

// readLine reads one line of at most limit bytes. raw holds every byte consumed (an LF
// left over from the previous line's CRLF included); content is the line without that
// LF and without its terminator. terminated is false when the stream ended first.
func (s *Reader) readLine(limit int) (raw, content []byte, terminated bool, err error) {
	if s.skipLF {
		s.skipLF = false
		b, err := s.r.ReadByte()
		if err != nil {
			return nil, nil, false, eofAsNil(err)
		}
		if b == '\n' {
			raw = append(raw, b)
		} else {
			_ = s.r.UnreadByte() // cannot fail right after a successful ReadByte
		}
	}
	start := len(raw)
	for {
		if len(raw) >= limit {
			return raw, raw[start:], false, ErrTooLarge
		}
		b, err := s.r.ReadByte()
		if err != nil {
			return raw, raw[start:], false, eofAsNil(err)
		}
		switch b {
		case '\n':
			return append(raw, b), raw[start:], true, nil
		case '\r':
			s.skipLF = true
			return append(raw, b), raw[start:], true, nil
		}
		raw = append(raw, b)
	}
}

// splitField splits an SSE line into field name and value. A line starting with ":"
// is a comment and has an empty field name.
func splitField(line []byte) (field, value []byte) {
	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		return line, nil
	}
	return field, bytes.TrimPrefix(value, []byte(" "))
}

// eofAsNil turns the end of the stream into "no error": readLine reports it through
// terminated instead.
func eofAsNil(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
