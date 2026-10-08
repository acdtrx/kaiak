package server

import (
	"context"
	"strconv"

	"kaiak/internal/provider"
)

// applyModelParams works out the parameter the model's config sets on a body request
// — the output limit, the one it sets (docs/specs/GATEWAY.md, Model metadata and
// Output limit) — and the request's effective output limit. The provider splices the
// parameters into the client's body; nothing else in it changes.
//
//   - Output limit out of range (chat, completions, Messages and Responses): a key the client set below 0
//     (llama-server reads -1 as unlimited, past any reservation) or above the
//     model's context_length (it can never be honored — prompt and output share the
//     context) is refused, 400 invalid_value naming the key, OpenAI's answer to the
//     same mistakes.
//   - Output limit (chat, completions, Messages and Responses, when the model declares one): a request
//     that sets no output-limit key gets the default under the endpoint's key,
//     lowered to leave the prompt room in the context (injectedOutputLimit); a key
//     set above the ceiling is lowered to the ceiling — each key the client set is
//     checked on its own.
func applyModelParams(_ context.Context, rq *request) *apiError {
	model := rq.snapshot.Models[rq.model]
	keys := rq.endpoint.outputLimitKeys
	if len(keys) == 0 {
		return nil
	}
	var effective *int64
	raise := func(n int64) {
		if effective == nil || n > *effective {
			effective = &n
		}
	}
	anySet := false
	for i, value := range rq.inbound.OutputLimits {
		if value == nil {
			continue
		}
		key := keys[i]
		anySet = true
		if *value < 0 {
			return errOutputLimitNegative(key, *value)
		}
		if *value > model.ContextLength {
			return errOutputLimitTooLarge(key, *value, model.ContextLength)
		}
		if limit := model.OutputLimit; limit != nil && *value > limit.Ceiling {
			if apiErr := fitsThinkingBudget(rq, key, limit.Ceiling, "ceiling"); apiErr != nil {
				return apiErr
			}
			rq.params = append(rq.params, outputLimitParam(key, limit.Ceiling))
			raise(limit.Ceiling)
		} else {
			raise(*value)
		}
	}
	if limit := model.OutputLimit; limit != nil && !anySet {
		n := injectedOutputLimit(limit.Default, model.ContextLength, rq.input.LargestPrompt)
		if apiErr := fitsThinkingBudget(rq, keys[0], n, "default"); apiErr != nil {
			return apiErr
		}
		rq.params = append(rq.params, outputLimitParam(keys[0], n))
		raise(n)
	}
	rq.outputLimit = effective
	return nil
}

// fitsThinkingBudget refuses an output limit n the gateway would set (the model's
// ceiling or default, named by which) at or below the request's thinking budget: the
// backend requires budget_tokens below max_tokens and would refuse the request over a
// value the client never sent (docs/specs/GATEWAY.md, Limits: output limit). The
// answer names the model's limit, so the operator sees what to raise.
func fitsThinkingBudget(rq *request, key string, n int64, which string) *apiError {
	budget := rq.inbound.ThinkingBudget
	if budget == nil || n > *budget {
		return nil
	}
	return errOutputLimitBelowThinking(key, n, which, *budget)
}

// minInjectedOutputLimit is the least an injected default is lowered to: the input
// estimate is rough (text bytes ÷ 4, a flat figure per media item), and a tiny limit
// would cut answers the backend could give.
const minInjectedOutputLimit = 256

// injectedOutputLimit is the output limit the gateway sets on a request that sets
// none: the model's default, lowered to the room the prompt leaves in the context
// (context length − the input one sequence sees: a batch's largest prompt, not the
// batch), but not below minInjectedOutputLimit — a
// default above the context minus a long prompt makes backends refuse the request
// outright (docs/specs/GATEWAY.md, Limits: output limit).
func injectedOutputLimit(def, contextLength, input int64) int64 {
	if contextLength <= 0 {
		return def
	}
	return min(def, max(contextLength-input, minInjectedOutputLimit))
}

func outputLimitParam(key string, n int64) provider.Param {
	return provider.Param{Key: key, Value: strconv.AppendInt(nil, n, 10)}
}
