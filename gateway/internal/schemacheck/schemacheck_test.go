package schemacheck

import (
	"errors"
	"testing"
)

func TestFirstDuplicateMember(t *testing.T) {
	for _, c := range []struct {
		name, doc, path string // path "" = no duplicate
	}{
		{"none", `{"a":1,"b":{"a":2},"c":[{"a":3},{"a":4}]}`, ""},
		{"scalar", `7`, ""},
		{"top level", `{"a":1,"b":2,"a":3}`, "/a"},
		{"nested object", `{"x":{"y":{"z":1,"z":1}}}`, "/x/y/z"},
		{"inside an array", `{"x":[1,{"k":1},{"k":1,"k":2}]}`, "/x/2/k"},
		{"array of arrays", `[[{}],[{"k":1,"k":1}]]`, "/1/0/k"},
		{"escaped spelling", `{"model":1,"mod\u0065l":2}`, "/model"},
		{"empty name", `{"":1,"":2}`, "/"},
		{"name needing escape in the pointer", `{"a/b~":{},"a/b~":{}}`, "/a~1b~0"},
		{"after a closed sibling", `{"a":{"b":1},"c":[],"a":0}`, "/a"},
		{"invalid JSON before a duplicate", `{"a":,"a":1}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, found := FirstDuplicateMember([]byte(c.doc))
			if found != (c.path != "") || path != c.path {
				t.Errorf("got %q, %v; want %q", path, found, c.path)
			}
		})
	}
}

func TestDecodeRefusesADuplicateMember(t *testing.T) {
	_, err := Decode([]byte(`{"a":[{"b":1,"b":2}]}`))
	var duplicate *DuplicateMemberError
	if !errors.As(err, &duplicate) || duplicate.Path != "/a/0/b" {
		t.Fatalf("err = %v, want a *DuplicateMemberError at /a/0/b", err)
	}
	if _, err := Decode([]byte(`{"a":1,"a"`)); errors.As(err, &duplicate) {
		t.Error("a syntax error reported as a duplicate")
	}
}
