package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"kaiak/internal/auth"
	"kaiak/internal/config"
	"kaiak/internal/provider"
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

	ContextLength    int64               `json:"context_length"`
	Capabilities     config.Capabilities `json:"capabilities"`
	ReasoningEfforts []string            `json:"reasoning_efforts"`
	// Endpoints are the body endpoints some deployment of the model serves, by their
	// metric names, sorted: which API reaches the model. Information from the config,
	// not health.
	Endpoints []string `json:"endpoints"`
}

// modelProps is /v1/models/{id}/props: the model entry plus what the gateway applies
// to a request's output — the output limit (null when none is declared).
type modelProps struct {
	modelEntry
	OutputLimit *config.OutputLimit `json:"output_limit"`
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
		ID:               m.Name,
		Object:           "model",
		Created:          modelCreated,
		OwnedBy:          "kaiak",
		ContextLength:    m.ContextLength,
		Capabilities:     m.Capabilities,
		ReasoningEfforts: efforts,
		Endpoints:        servedEndpoints(m),
	}
}

// servedEndpoints names the body endpoints at least one of m's deployments serves,
// sorted (docs/specs/GATEWAY.md, Client API → /v1/models).
func servedEndpoints(m *config.Model) []string {
	names := []string{}
	for _, ep := range bodyEndpoints {
		if slices.ContainsFunc(m.Deployments, func(d config.Deployment) bool { return serves(d, ep.api) }) {
			names = append(names, ep.name)
		}
	}
	slices.Sort(names)
	return names
}

// anthropicModelEntry is one model as the Anthropic-shaped list describes it
// (docs/specs/GATEWAY.md, Client API → Anthropic-shaped model list): Anthropic's
// fields, then kaiak's metadata.
type anthropicModelEntry struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`

	ContextLength    int64               `json:"context_length"`
	Capabilities     config.Capabilities `json:"capabilities"`
	ReasoningEfforts []string            `json:"reasoning_efforts"`
	Endpoints        []string            `json:"endpoints"`
}

// anthropicModelList is Anthropic's model list: always one page. first_id and last_id
// are null for an empty list.
type anthropicModelList struct {
	Data    []anthropicModelEntry `json:"data"`
	HasMore bool                  `json:"has_more"`
	FirstID *string               `json:"first_id"`
	LastID  *string               `json:"last_id"`
}

// anthropicModelCreated is every Anthropic-shaped entry's created_at: modelCreated as
// an RFC 3339 time.
const anthropicModelCreated = "1970-01-01T00:00:00Z"

func newAnthropicModelEntry(m *config.Model) anthropicModelEntry {
	e := newModelEntry(m)
	return anthropicModelEntry{
		Type: "model", ID: e.ID, DisplayName: e.ID, CreatedAt: anthropicModelCreated,
		ContextLength: e.ContextLength, Capabilities: e.Capabilities, ReasoningEfforts: e.ReasoningEfforts,
		Endpoints: e.Endpoints,
	}
}

// servesMessages reports whether some deployment of m serves Messages: the models an
// Anthropic client can use.
func servesMessages(m *config.Model) bool {
	return slices.ContainsFunc(m.Deployments, func(d config.Deployment) bool { return serves(d, provider.Messages) })
}

// answerAnthropicModels answers the Anthropic-shaped model list and entry, over the
// models the key may use that some deployment serves through Messages. A model without
// Messages is as unknown here as one that does not exist.
func answerAnthropicModels(rq *request) *apiError {
	if rq.endpoint == endpointGetModel {
		m := rq.snapshot.Models[rq.model]
		if !servesMessages(m) {
			return authError(auth.ModelNotFound(rq.model))
		}
		writeJSON(rq.w, newAnthropicModelEntry(m))
		return nil
	}
	list := anthropicModelList{Data: []anthropicModelEntry{}}
	for _, name := range rq.identity.AllowedModels() {
		if m := rq.snapshot.Models[name]; servesMessages(m) {
			list.Data = append(list.Data, newAnthropicModelEntry(m))
		}
	}
	if n := len(list.Data); n > 0 {
		list.FirstID, list.LastID = &list.Data[0].ID, &list.Data[n-1].ID
	}
	writeJSON(rq.w, list)
	return nil
}

// answerModelEndpoint is the model endpoints' terminal stage (modelRequests).
// model_access has checked a named model exists and is allowed.
func answerModelEndpoint(_ context.Context, rq *request) *apiError {
	if rq.endpoint.answersAnthropic(rq.r) {
		return answerAnthropicModels(rq)
	}
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
		writeJSON(rq.w, modelProps{modelEntry: newModelEntry(m), OutputLimit: m.OutputLimit})
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
