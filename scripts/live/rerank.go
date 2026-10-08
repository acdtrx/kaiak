package main

// The rerank checks, for the kinds serving rerank (vllm, llama-server), and the
// wrong-endpoint checks: a request to a deployment whose server lacks the endpoint.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Request IDs of the rerank checks' requests whose log lines they read.
const (
	idRerank         = "live-rerank"
	idRerankCap      = "live-rerank-cap"
	idRerankOversize = "live-rerank-oversize"
	idChatToReranker = "live-chat-to-reranker"
	idRerankAfter    = "live-rerank-after"
	idRerankToChat   = "live-rerank-to-chat"
	idChatAfter      = "live-chat-after"
)

// rerankQuery and relevanceDocuments: the document at relevantIndex answers the
// query, the one at irrelevantIndex has nothing to do with it. The irrelevant one
// comes first, so a reranker that kept the order fails the relevance check.
const rerankQuery = "What does a lighthouse keeper do?"

var relevanceDocuments = []string{
	"Knead the dough for ten minutes, then let it rise in a warm place until doubled.",
	"A lighthouse keeper tends the lamp, logs the weather and watches the sea for ships in trouble.",
}

const irrelevantIndex, relevantIndex = 0, 1

// oversizeDocument is longer than any reranker's context — over 200,000 tokens, six
// times a 32k context — and about a quarter of the gateway's default 4 MiB body cap.
var oversizeDocument = strings.Repeat("The keeper climbs the tower at dusk and lights the lamp. ", 18000)

// endpointMissingWarning is the gateway's warning when a deployment's server does not
// serve an endpoint its type serves, once per probe interval and deployment.
const endpointMissingWarning = "the deployment's server does not serve an endpoint its type serves"

// rerankBody is a rerank request to model: rerankQuery over documents.
func rerankBody(model string, documents []string) map[string]any {
	return map[string]any{"model": model, "query": rerankQuery, "documents": documents}
}

// rerankChecks are the rerank checks, for the kinds that serve rerank: those needing
// the reranker server (-rerank-base-url), then a rerank request to the chat model.
func (r *run) rerankChecks() {
	if r.o.reranker() {
		r.checkRerank()
		r.checkRerankUsage()
		r.checkRerankCap()
		r.checkRerankOversize()
		r.checkChatToReranker()
	} else {
		r.skip("rerank", "no -rerank-base-url")
	}
	r.checkRerankToChat()
}

// rerankAnswer is the part of a rerank answer the checks read, in either server's
// shape: both carry the model, usage and one result per document with its index and
// relevance score.
type rerankAnswer struct {
	Model   string
	Results []struct {
		Index          *int
		RelevanceScore *float64 `json:"relevance_score"`
	}
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
	}
}

// checkRerank reranks relevanceDocuments: an answer under the public model name with
// a result per document and usage (rerank), the relevant document above the
// irrelevant one (rerank-relevance) — coarse, since scores vary by model and template.
func (r *run) checkRerank() {
	const name, relevance = "rerank", "rerank-relevance"
	resp, err := r.post(r.key, "/v1/rerank", idRerank, rerankBody(modelRerank, relevanceDocuments))
	if !r.ok(name, resp, err, idRerank) {
		r.skip(relevance, "no rerank answer")
		return
	}
	var a rerankAnswer
	if err := json.Unmarshal(resp.body, &a); err != nil || len(a.Results) == 0 {
		r.fail(name, "no results: %s", clip(string(resp.body)))
		r.skip(relevance, "no rerank answer")
		return
	}
	scores := map[int]float64{}
	for _, res := range a.Results {
		if res.Index != nil && res.RelevanceScore != nil {
			scores[*res.Index] = *res.RelevanceScore
		}
	}
	switch {
	case a.Model != modelRerank:
		r.fail(name, "answer names model %q, want the public name %q", a.Model, modelRerank)
	case len(a.Results) != len(relevanceDocuments) || len(scores) != len(relevanceDocuments):
		r.fail(name, "want a result with index and relevance_score for each of %d documents: %s",
			len(relevanceDocuments), clip(string(resp.body)))
	case a.Usage == nil || a.Usage.PromptTokens == 0:
		r.fail(name, "no prompt_tokens in usage: %s", clip(string(resp.body)))
	default:
		r.pass(name, fmt.Sprintf("%d results under %s, %d prompt tokens", len(a.Results), modelRerank, a.Usage.PromptTokens))
	}
	relevant, scoredRelevant := scores[relevantIndex]
	irrelevant, scoredIrrelevant := scores[irrelevantIndex]
	switch {
	case !scoredRelevant || !scoredIrrelevant:
		r.fail(relevance, "the results do not score both documents: %s", clip(string(resp.body)))
	case relevant <= irrelevant:
		r.fail(relevance, "the relevant document scored %.4g, the irrelevant one %.4g: check the server's score template "+
			"(docs/DEPLOYMENT.md, Rerankers)", relevant, irrelevant)
	default:
		r.pass(relevance, fmt.Sprintf("relevant document %.4g, irrelevant %.4g", relevant, irrelevant))
	}
}

// checkRerankUsage reads the rerank request's log line: operation rerank, and the
// server's own token count (checkUsageLog).
func (r *run) checkRerankUsage() {
	const name = "usage-log/rerank"
	line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", idRerank))
	if err == nil && line["gen_ai.operation.name"] != "rerank" {
		r.fail(name, "gen_ai.operation.name %v, want rerank", line["gen_ai.operation.name"])
		return
	}
	r.checkUsageLog(name, idRerank, false)
}

// checkRerankCap sends one document more than global.max_rerank_documents: refused
// before routing.
func (r *run) checkRerankCap() {
	const name = "rerank-cap"
	documents := make([]string, rerankDocumentsCap+1)
	for i := range documents {
		documents[i] = fmt.Sprintf("Document %d.", i+1)
	}
	resp, err := r.post(r.key, "/v1/rerank", idRerankCap, rerankBody(modelRerank, documents))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	if problem := openAIError(resp, http.StatusBadRequest, "invalid_value", "documents"); problem != "" {
		r.fail(name, "%s", problem)
		return
	}
	if sent, err := r.notSent(idRerankCap); err != nil || !sent {
		r.fail(name, "refused, but the log line shows it routed to a backend (%v)", err)
		return
	}
	r.pass(name, fmt.Sprintf("%d documents over max_rerank_documents %d → 400 invalid_value on documents, never sent",
		len(documents), rerankDocumentsCap))
}

// checkRerankOversize sends one document longer than any reranker's context: the
// server refuses the pair with a 400, which the gateway relays as the caller's error.
// A 5xx is the server failing on it, which counts toward the circuit — llama-server
// answers 500 for a pair that does not fit its batch.
func (r *run) checkRerankOversize() {
	const name = "rerank-oversize"
	resp, err := r.post(r.key, "/v1/rerank", idRerankOversize, rerankBody(modelRerank, []string{oversizeDocument}))
	if err != nil {
		r.fail(name, "%v", err)
		return
	}
	var e struct {
		Error struct{ Message, Type string }
	}
	_ = json.Unmarshal(resp.body, &e)
	size := fmt.Sprintf("a %.1f MB document", float64(len(oversizeDocument))/1e6)
	switch {
	case resp.status >= 500:
		r.fail(name, "%s → status %d: the server failed on a pair longer than its context instead of refusing it, "+
			"and the gateway counts that toward the circuit — llama-server: its batch size, docs/DEPLOYMENT.md → Rerankers%s",
			size, resp.status, r.upstreamHint(idRerankOversize))
	case resp.status != http.StatusBadRequest:
		r.fail(name, "%s → status %d, want 400: %s", size, resp.status, clip(string(resp.body)))
	case e.Error.Message == "":
		r.fail(name, "%s → 400 without an error message: %s", size, clip(string(resp.body)))
	default:
		line, err := r.gw.logs.wait(r.ctx, msg("request", "kaiak.request.id", idRerankOversize))
		if err != nil || line["kaiak.backend.id"] != backendRerank {
			r.fail(name, "400 %s from the gateway, not from %s (%v): %s", e.Error.Type, backendRerank, err, clip(e.Error.Message))
			return
		}
		r.pass(name, fmt.Sprintf("%s → 400 %s from the server: %q", size, e.Error.Type, clip(e.Error.Message)))
	}
}

// checkChatToReranker sends a chat request to the reranker. vLLM creates its routes
// from the model it loaded, so a reranker's server has no chat route: the gateway
// answers 502 upstream_endpoint_missing, neutral for the circuit — a rerank right after
// is served. llama-server registers every route whatever its flags, so there a chat
// to a reranker is no endpoint missing (docs/specs/GATEWAY.md, Providers → An endpoint
// missing from a server).
func (r *run) checkChatToReranker() {
	const name = "chat-to-reranker"
	if r.o.kind != kindVLLM {
		r.skip(name, r.o.kind+" registers every route whatever its model: no endpoint missing to check")
		return
	}
	resp, err := r.post(r.key, "/v1/chat/completions", idChatToReranker, r.chatBody(modelRerank, false, nil))
	if _, problem := r.endpointMissing(resp, err, idChatToReranker, epChat, backendRerank); problem != "" {
		r.fail(name, "chat to %s: %s", modelRerank, problem)
		return
	}
	if problem := r.servedAfter(idRerankAfter, "/v1/rerank", rerankBody(modelRerank, relevanceDocuments)); problem != "" {
		r.fail(name, "a rerank right after: %s", problem)
		return
	}
	r.pass(name, "chat → 502 upstream_endpoint_missing, warned for "+backendRerank+"; a rerank right after → 200")
}

// checkRerankToChat sends a rerank request to the chat model, whose server serves no
// rerank: vLLM's chat server has no rerank route, and llama-server started without
// --reranking answers 501. The gateway answers 502 upstream_endpoint_missing, neutral
// for the circuit — a chat right after is served.
func (r *run) checkRerankToChat() {
	const name = "rerank-to-chat"
	chatBackends := []string{backendFirst}
	if r.o.twoBackends() {
		chatBackends = append(chatBackends, backendSecond)
	}
	resp, err := r.post(r.key, "/v1/rerank", idRerankToChat, rerankBody(modelChat, relevanceDocuments))
	warned, problem := r.endpointMissing(resp, err, idRerankToChat, epRerank, chatBackends...)
	if problem != "" {
		r.fail(name, "rerank to %s: %s", modelChat, problem)
		return
	}
	if problem := r.servedAfter(idChatAfter, "/v1/chat/completions", r.chatBody(modelChat, false, nil)); problem != "" {
		r.fail(name, "a chat right after: %s", problem)
		return
	}
	r.pass(name, "rerank → 502 upstream_endpoint_missing, warned for "+warned+"; a chat right after → 200")
}

// endpointMissing checks the answer to a request whose deployment's server lacks the
// endpoint: 502 upstream_endpoint_missing, and the gateway's warning for the request
// naming the endpoint and one of backends. It returns the backend warned for, or what
// is wrong.
func (r *run) endpointMissing(resp *response, err error, id, endpoint string, backends ...string) (warned, problem string) {
	if err != nil {
		return "", err.Error()
	}
	if problem := openAIError(resp, http.StatusBadGateway, "upstream_endpoint_missing"); problem != "" {
		return "", problem + r.upstreamHint(id)
	}
	line, err := r.gw.logs.wait(r.ctx, msg(endpointMissingWarning, "kaiak.request.id", id, "kaiak.endpoint", endpoint))
	if err != nil {
		return "", fmt.Sprintf("no warning %q for the request: %v", endpointMissingWarning, err)
	}
	warned = fmt.Sprint(line["kaiak.backend.id"])
	if !slices.Contains(backends, warned) {
		return "", fmt.Sprintf("the warning names backend %s, want %s", warned, strings.Join(backends, " or "))
	}
	return warned, ""
}

// servedAfter sends body to path and requires 200: the deployment that lacked another
// endpoint was not taken out of service. "" when served, else what is wrong.
func (r *run) servedAfter(id, path string, body map[string]any) string {
	resp, err := r.post(r.key, path, id, body)
	switch {
	case err != nil:
		return err.Error()
	case resp.status != http.StatusOK:
		return fmt.Sprintf("status %d, want 200: %s%s", resp.status, clip(string(resp.body)), r.upstreamHint(id))
	}
	return ""
}
