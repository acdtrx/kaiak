// Package control is the gateway's side of the control protocol
// (docs/specs/CONTROL-PROTOCOL.md) and the only code that talks to a control plane.
// It holds the messages — their Go types, and strict decoding with validation that
// mirrors protocol/schema/, kept in agreement with kaiak-control by the shared
// fixtures in protocol/fixtures/messages — and the client: it boots and follows the
// config (client.go), sends usage batches queued in memory (usage.go, queue.go) and
// reports status (status.go).
package control

// ProtocolVersion is the protocol version this gateway speaks, sent in the
// Kaiak-Protocol header and in status.
const ProtocolVersion = 5

// Message rule codes: what the schemas cannot express. They are part of the contract;
// kaiak-control reports the same code for the same message. A rejected message is a
// *schemacheck.ValidationError (with schemacheck's stage codes for syntax, duplicate
// members and schema violations); a timestamp naming no real instant is
// config.CodeTimestampInvalid, as in a config document.
const (
	CodeTotalsWindowDuplicate        = "totals-window-duplicate"
	CodeCountedThroughEpochDuplicate = "counted-through-epoch-duplicate"
	CodeRecordInstanceMismatch       = "record-instance-mismatch"
	CodeRecordIDDuplicate            = "record-id-duplicate"
)
