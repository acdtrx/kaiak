package capture

// SessionHeader is the request header naming the conversation a request belongs to
// (docs/specs/GATEWAY.md, Request capture → Sessions): read only when the request is
// captured, never forwarded, echoed or logged.
const SessionHeader = "Kaiak-Session-Id"

// maxSessionID is the longest valid session ID.
const maxSessionID = 256

// Why a session ID was ignored: kaiak.session_id.ignored on the span and the request
// line.
const (
	ignoredEmpty             = "empty"
	ignoredTooLong           = "too_long"
	ignoredInvalidCharacters = "invalid_characters"
)

// session is the session the values of a request's Kaiak-Session-Id headers name,
// read from the first as Request IDs read x-request-id: its ID when valid — 1 to
// maxSessionID bytes of visible ASCII, '!' to '~' — else no session and why it was
// ignored, judged in the order empty, too long, invalid characters. With no header
// both are "". A malformed value never refuses the request.
func session(values []string) (id, ignored string) {
	if len(values) == 0 {
		return "", ""
	}
	v := values[0]
	switch {
	case v == "":
		return "", ignoredEmpty
	case len(v) > maxSessionID:
		return "", ignoredTooLong
	}
	for i := range len(v) {
		if v[i] < '!' || v[i] > '~' {
			return "", ignoredInvalidCharacters
		}
	}
	return v, ""
}
