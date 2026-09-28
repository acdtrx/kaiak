package accounting

import "testing"

// scanBoth scans body whole and one byte at a time, and checks both scans agree.
func scanBoth(t *testing.T, body string) *memberScanner {
	t.Helper()
	whole := newMemberScanner(map[string]int{"usage": 1024, "choices": 1024})
	whole.feed([]byte(body))
	bytewise := newMemberScanner(map[string]int{"usage": 1024, "choices": 1024})
	for i := range len(body) {
		bytewise.feed([]byte{body[i]})
	}
	for _, name := range []string{"usage", "choices"} {
		a, okA := whole.member(name)
		b, okB := bytewise.member(name)
		if okA != okB || string(a) != string(b) {
			t.Fatalf("%s: whole %q %v, bytewise %q %v", name, a, okA, b, okB)
		}
	}
	return whole
}

func TestScannerFindsTopLevelMembers(t *testing.T) {
	cases := []struct {
		name, body, usage, choices string
	}{
		{"usage last",
			`{"id":"x","choices":[{"text":"a"}],"usage":{"prompt_tokens":3}}`,
			`{"prompt_tokens":3}`, `[{"text":"a"}]`},
		{"usage first, whitespace everywhere",
			" {\n \"usage\" : {\"prompt_tokens\": 3} ,\t\"choices\" :[] }",
			`{"prompt_tokens": 3}`, `[]`},
		{"nested usage keys are not top-level",
			`{"data":[{"usage":{"prompt_tokens":99}}],"meta":{"usage":1},"usage":{"prompt_tokens":1}}`,
			`{"prompt_tokens":1}`, ""},
		{"strings with braces, quotes and escapes",
			`{"model":"a\"}{,\\","choices":[{"text":"}\"]\\"}],"usage":{"prompt_tokens":2}}`,
			`{"prompt_tokens":2}`, `[{"text":"}\"]\\"}]`},
		{"escaped key", `{"usage":{"prompt_tokens":4}}`, `{"prompt_tokens":4}`, ""},
		{"scalar values", `{"created":17,"ok":true,"usage":null,"choices":"x"}`, `null`, `"x"`},
		{"repeated member: last wins", `{"usage":{"prompt_tokens":1},"usage":{"prompt_tokens":2}}`,
			`{"prompt_tokens":2}`, ""},
		{"not an object", `[{"usage":{"prompt_tokens":1}}]`, "", ""},
		{"body cut short", `{"choices":[{"text":"abc"`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := scanBoth(t, c.body)
			for name, want := range map[string]string{"usage": c.usage, "choices": c.choices} {
				got, ok := s.member(name)
				if want == "" {
					if ok {
						t.Errorf("%s = %q, want none", name, got)
					}
					continue
				}
				if !ok || string(got) != want {
					t.Errorf("%s = %q %v, want %q", name, got, ok, want)
				}
			}
		})
	}
}

func TestScannerBoundsWhatItKeeps(t *testing.T) {
	s := newMemberScanner(map[string]int{"choices": 8})
	s.feed([]byte(`{"choices":[{"text":"long enough"}],"usage":{}}`))
	if v, ok := s.member("choices"); ok {
		t.Errorf("over-limit member kept whole: %q", v)
	}
	if n := s.memberSize("choices"); n != len(`[{"text":"long enough"}]`) {
		t.Errorf("size %d", n)
	}
	if _, ok := s.member("usage"); ok {
		t.Error("an unwanted member was kept")
	}
}
