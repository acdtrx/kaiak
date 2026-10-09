package server

import (
	"context"
	"fmt"
	"strconv"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/provider"
)

// applyModelParams works out the parameter the model's config sets on a body request
// — the output limit, the one it sets (docs/specs/GATEWAY.md, Model metadata and
// Output limit) — and the request's effective output limit. The provider splices the
// parameters into the client's body; nothing else in it changes.
//
//   - Output limit out of range (every endpoint with an output-limit key: chat,
//     completions, Messages, Responses, speech, the speech batch): a key the client
//     set below 0 (llama-server reads -1 as unlimited, past any reservation) or above
//     the model's context_length (it can never be honored — prompt and output share
//     the context) is refused, 400 invalid_value naming the key, OpenAI's answer to
//     the same mistakes.
//   - Output limit (those endpoints, when the model declares one): a request
//     that sets no output-limit key gets the default under the endpoint's key,
//     lowered to leave the prompt room in the context (injectedOutputLimit); a key
//     set above the ceiling is lowered to the ceiling — each key the client set is
//     checked on its own.
//   - A speech batch's items (applyItemOutputLimits): each item's own max_new_tokens
//     under the same rules, else the batch's.
//
// The output the request may generate in all (outputReserve) is its effective output
// limit once per sequence it asks for.
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
	if rq.endpoint.api.Format() == provider.FormatSpeechBatch {
		return applyItemOutputLimits(rq, model, keys[0], effective)
	}
	rq.outputLimit = effective
	if effective != nil {
		rq.outputReserve = accounting.SaturatingMul(*effective, rq.inbound.Sequences)
	}
	return nil
}

// applyItemOutputLimits works out a speech batch's items' output limits
// (docs/specs/GATEWAY.md, Limits → Output-limit keys): an item's own key is checked as
// the batch's is — refused below 0 or above the context, lowered to the ceiling in the
// item — and an item without one gets the batch's effective limit (batch, the default
// included), as vLLM-Omni merges each item with the batch. The items are the batch's
// sequences: its effective output limit is its largest item's, and the reservation
// counts each item's own once; an item with no limit — none sent, none declared —
// reserves nothing, as an unbounded request does.
func applyItemOutputLimits(rq *request, model *config.Model, key string, batch *int64) *apiError {
	var largest *int64
	var reserve int64
	for i, value := range rq.inbound.ItemOutputLimits {
		limit := batch
		if value != nil {
			param := fmt.Sprintf("items[%d].%s", i, key)
			if *value < 0 {
				return errOutputLimitNegative(param, *value)
			}
			if *value > model.ContextLength {
				return errOutputLimitTooLarge(param, *value, model.ContextLength)
			}
			n := *value
			if declared := model.OutputLimit; declared != nil && n > declared.Ceiling {
				n = declared.Ceiling
				rq.itemParams = append(rq.itemParams, provider.ItemParam{List: "items", Index: i, Param: outputLimitParam(key, n)})
			}
			limit = &n
		}
		if limit == nil {
			continue
		}
		reserve = accounting.SaturatingAdd(reserve, *limit)
		if largest == nil || *limit > *largest {
			largest = limit
		}
	}
	rq.outputLimit, rq.outputReserve = largest, reserve
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
