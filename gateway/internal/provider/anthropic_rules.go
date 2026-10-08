package provider

import (
	"strings"
)

// Rules both Anthropic types share (anthropic.go, azure_anthropic.go).

// anthropicVersion is the anthropic-version header both Anthropic types send: the
// API's one version (docs/specs/GATEWAY.md, Base URLs).
const anthropicVersion = "2023-06-01"

// anthropicModelMissing is Anthropic's missing model: a not_found_error whose message
// begins "model:" (the API names the model it lacks so; a file or other object it
// lacks reads otherwise).
func anthropicModelMissing(answer []byte, _ string) bool {
	f, ok := readNotFoundFields(answer)
	return ok && f.errorType == "not_found_error" && strings.HasPrefix(f.message, "model:")
}
