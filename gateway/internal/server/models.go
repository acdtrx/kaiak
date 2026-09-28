package server

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"

	"kaiak/internal/config"
)

// The model endpoints answer from the declared config alone; field names and shapes
// are docs/specs/GATEWAY.md's (Client API, model endpoints).

// modelEntry is one model as /v1/models and /v1/models/{id} describe it: OpenAI's
// fields, then kaiak's metadata. OpenAI clients ignore the extra fields.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	ContextLength    int64            `json:"context_length"`
	Capabilities     capabilitiesJSON `json:"capabilities"`
	ReasoningEfforts []string         `json:"reasoning_efforts"`
}

type capabilitiesJSON struct {
	Streaming bool `json:"streaming"`
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Reasoning bool `json:"reasoning"`
}

// modelProps is /v1/models/{id}/props: the model entry plus what the gateway applies
// to requests — declared defaults and the output limit (null when none is declared).
type modelProps struct {
	modelEntry
	Defaults    map[string]json.RawMessage `json:"defaults"`
	OutputLimit *outputLimitJSON           `json:"output_limit"`
}

type outputLimitJSON struct {
	Default int64 `json:"default"`
	Ceiling int64 `json:"ceiling"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// modelCreated is every entry's "created" time. Models are declared, not created at a
// moment the gateway knows; a fixed value keeps listings identical across reloads and
// replicas.
const modelCreated = 0

func newModelEntry(m *config.Model) modelEntry {
	efforts := m.ReasoningEfforts
	if efforts == nil {
		efforts = []string{}
	}
	return modelEntry{
		ID:            m.Name,
		Object:        "model",
		Created:       modelCreated,
		OwnedBy:       "kaiak",
		ContextLength: m.ContextLength,
		Capabilities: capabilitiesJSON{
			Streaming: m.Capabilities.Streaming,
			Tools:     m.Capabilities.Tools,
			Vision:    m.Capabilities.Vision,
			Reasoning: m.Capabilities.Reasoning,
		},
		ReasoningEfforts: efforts,
	}
}

// answerModelEndpoint is the terminal stage for the model endpoints; the body
// endpoints never reach it (the provider stage answers them). model_access has
// checked a named model exists and is allowed.
func answerModelEndpoint(_ context.Context, rq *request) *apiError {
	switch rq.endpoint {
	case endpointListModels:
		names := rq.identity.AllowedModels()
		list := modelList{Object: "list", Data: make([]modelEntry, 0, len(names))}
		for _, name := range names {
			list.Data = append(list.Data, newModelEntry(rq.snapshot.Models[name]))
		}
		writeJSON(rq.w, list)
	case endpointGetModel:
		writeJSON(rq.w, newModelEntry(rq.snapshot.Models[rq.model]))
	case endpointModelProps:
		m := rq.snapshot.Models[rq.model]
		props := modelProps{modelEntry: newModelEntry(m), Defaults: maps.Clone(m.Defaults)}
		if props.Defaults == nil {
			props.Defaults = map[string]json.RawMessage{}
		}
		if m.OutputLimit != nil {
			props.OutputLimit = &outputLimitJSON{Default: m.OutputLimit.Default, Ceiling: m.OutputLimit.Ceiling}
		}
		writeJSON(rq.w, props)
	}
	return nil
}

// writeJSON writes v as a 200 JSON response. Names stay as written (no HTML escaping).
func writeJSON(w http.ResponseWriter, v any) {
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // plain structs and config-validated JSON: encoding cannot fail
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data.Bytes()) // a failed write means the client left; nothing to do
}
