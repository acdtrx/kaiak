package clip

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestString(t *testing.T) {
	short := strings.Repeat("a", Max)
	if got := String(short); got != short {
		t.Errorf("a string of Max bytes changed: %d bytes", len(got))
	}
	long := strings.Repeat("b", 1<<20)
	if got := String(long); got != strings.Repeat("b", Max)+Marker {
		t.Errorf("1 MiB string clipped to %d bytes", len(got))
	}
	// A multi-byte character straddling the cut is dropped whole.
	multi := strings.Repeat("a", Max-1) + "é" + "tail"
	got := String(multi)
	if got != strings.Repeat("a", Max-1)+Marker || !utf8.ValidString(got) {
		t.Errorf("clipped %q", got)
	}
}
