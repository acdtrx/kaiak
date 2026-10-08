package schemacheck

import (
	"errors"
	"slices"
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

func TestValidateRefusesADuplicateMember(t *testing.T) {
	noIssues := func(any) []Issue { return nil }
	_, err := Validate("doc", []byte(`{"a":[{"b":1,"b":2}]}`), noIssues)
	invalid, ok := errors.AsType[*ValidationError](err)
	if !ok || !slices.Equal(invalid.Codes(), []string{CodeDuplicateMember}) || invalid.Issues[0].Path != "/a/0/b" {
		t.Fatalf("err = %v, want a %s rejection at /a/0/b", err, CodeDuplicateMember)
	}
	_, err = Validate("doc", []byte(`{"a":1,"a"`), noIssues)
	if invalid, ok := errors.AsType[*ValidationError](err); !ok || !slices.Equal(invalid.Codes(), []string{CodeSyntax}) {
		t.Errorf("err = %v: a syntax error reported as a duplicate", err)
	}
}
