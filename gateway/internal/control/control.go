// Package control is the gateway's side of the control protocol
// (docs/specs/CONTROL-PROTOCOL.md) and the only code that talks to a control plane.
// It holds the messages — their Go types, and strict decoding with validation that
// mirrors protocol/schema/, kept in agreement with kaiak-control by the shared
// fixtures in protocol/fixtures/messages — and the client: it boots and follows the
// config (client.go), sends usage batches through a spool in the data directory
// (usage.go, spool.go) and reports status (status.go).
package control

import (
	"fmt"
	"slices"
	"strings"
)

// ProtocolVersion is the protocol version this gateway speaks, sent in the
// Kaiak-Protocol header and in status.
const ProtocolVersion = 5

// Codes for a rejected message. A schema violation has no per-rule code: CodeSchema
// plus the walker's own message, as for config documents.
const (
	// CodeSyntax: the body is not a single JSON value.
	CodeSyntax = "syntax"
	// CodeDuplicateMember: an object in the body names the same member twice.
	CodeDuplicateMember = "duplicate-member"
	// CodeSchema: the message breaks its schema.
	CodeSchema = "schema"
)

// Message rule codes: what the schemas cannot express. They are part of the contract;
// kaiak-control reports the same code for the same message.
const (
	CodeTimestampInvalid             = "timestamp-invalid"
	CodeTotalsWindowDuplicate        = "totals-window-duplicate"
	CodeCountedThroughEpochDuplicate = "counted-through-epoch-duplicate"
	CodeRecordInstanceMismatch       = "record-instance-mismatch"
	CodeRecordIDDuplicate            = "record-id-duplicate"
)

// Issue is one reason a message was rejected.
type Issue struct {
	Code string
	// Path is a JSON Pointer (RFC 6901) into the message; "" is the root.
	Path    string
	Message string
}

func (i Issue) String() string {
	path := i.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s [%s]", path, i.Message, i.Code)
}

// ValidationError rejects a message and lists every issue of the stage that failed
// (syntax, then duplicate members, then schema, then the message rules).
type ValidationError struct {
	// Message names the message kind, e.g. "usage ack".
	Message string
	Issues  []Issue
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.String()
	}
	return e.Message + " rejected: " + strings.Join(parts, "; ")
}

// Codes returns the distinct issue codes, sorted.
func (e *ValidationError) Codes() []string {
	codes := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		codes = append(codes, issue.Code)
	}
	slices.Sort(codes)
	return slices.Compact(codes)
}
