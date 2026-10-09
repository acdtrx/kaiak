package accounting

import "encoding/json"

// rerankUsage reads rerank answers (docs/specs/GATEWAY.md, Accounting → rerank usage):
// a body's top-level usage, as for embeddings — prompt_tokens as tokens_in, nothing
// else; a usage without prompt_tokens is no report (promptOnlyUnits). A rerank answer
// generates nothing, so one without a report estimates no output; it is never a
// stream, and an event stream answering a rerank request carries nothing the meter
// reads.
type rerankUsage struct {
	latestReport
}

func (u *rerankUsage) streamEvent([]byte) {}

// contentMember: none — nothing is generated.
func (u *rerankUsage) contentMember() string { return "" }

func (u *rerankUsage) bodyUsage(raw json.RawMessage) { u.keep(promptOnlyUnits(raw)) }

func (u *rerankUsage) bodyContentBytes(json.RawMessage) int64 { return 0 }

func (u *rerankUsage) streamContentBytes() int64 { return 0 }
