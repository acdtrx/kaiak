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
//   - Output limit out of range (chat, completions and Messages): a key the client set below 0
//     (llama-server reads -1 as unlimited, past any reservation) or above the
//     model's context_length (it can never be honored — prompt and output share the
//     context) is refused, 400 invalid_value naming the key, OpenAI's answer to the
//     same mistakes.
//   - Output limit (chat, completions and Messages, when the model declares one): a request
//     that sets no output-limit key gets the default under the endpoint's key,
//     lowered to leave the prompt room in the context (injectedOutputLimit); a key
//     set above the ceiling is lowered to the ceiling — each key the client set is
//     checked on its own.
func applyModelParams(_ context.Context, rq *request) *apiError {
	if !rq.endpoint.takesBody() {
		return nil
	}
	model := rq.snapshot.Models[rq.model]
	keys := outputLimitKeys(rq.endpoint)
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
	for _, k := range keys {
		value := k.value(&rq.inbound)
		if value == nil {
			continue
		}
		anySet = true
		if *value < 0 {
			return errOutputLimitNegative(k.name, *value)
		}
		if *value > model.ContextLength {
			return errOutputLimitTooLarge(k.name, *value, model.ContextLength)
		}
		if limit := model.OutputLimit; limit != nil && *value > limit.Ceiling {
			rq.params = append(rq.params, outputLimitParam(k.name, limit.Ceiling))
			raise(limit.Ceiling)
		} else {
			raise(*value)
		}
	}
	if limit := model.OutputLimit; limit != nil && !anySet {
		n := injectedOutputLimit(limit.Default, model.ContextLength, rq.input.LargestPrompt)
		rq.params = append(rq.params, outputLimitParam(keys[0].name, n))
		raise(n)
	}
	rq.outputLimit = effective
	return nil
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

// outputLimitKey is a request parameter capping the tokens a request generates.
type outputLimitKey struct {
	name  string
	value func(*inboundFields) *int64
}

// outputLimitKeys lists the endpoint's output-limit parameters; the first is the one a
// default is set under. Chat takes max_completion_tokens, OpenAI's current field,
// which vLLM, SGLang, llama-server, OpenAI and Azure all read — and which OpenAI's and
// Azure's reasoning models require (they refuse max_tokens) — and still honors the
// older max_tokens. Completions and Messages have only max_tokens (Messages requires
// it, so a model with an output limit always sends one). Embeddings and the
// token-counting endpoints generate nothing.
func outputLimitKeys(ep endpoint) []outputLimitKey {
	switch ep {
	case endpointChatCompletions:
		return []outputLimitKey{
			{"max_completion_tokens", func(f *inboundFields) *int64 { return f.MaxCompletionTokens }},
			{"max_tokens", func(f *inboundFields) *int64 { return f.MaxTokens }},
		}
	case endpointCompletions, endpointMessages:
		return []outputLimitKey{{"max_tokens", func(f *inboundFields) *int64 { return f.MaxTokens }}}
	}
	return nil
}

func outputLimitParam(key string, n int64) provider.Param {
	return provider.Param{Key: key, Value: strconv.AppendInt(nil, n, 10)}
}
