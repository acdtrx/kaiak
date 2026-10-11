package main

// The rerank checks, for the kinds serving rerank (vllm, llama-server), and the
// wrong-endpoint checks: a request for an endpoint the model's server lacks, which
// the model's endpoints list leaves out.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Request IDs of the rerank checks' requests whose log lines they read.
const (
	idRerank         = "live-rerank"
	idRerankCap      = "live-rerank-cap"
	idRerankOversize = "live-rerank-oversize"
	idChatToReranker = "live-chat-to-reranker"
	idRerankToChat   = "live-rerank-to-chat"
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

// checkChatToReranker sends a chat request to the reranker, whose server has no chat
// to answer it — vLLM creates no chat route for a reranker, and llama-server's would
// answer from a model that does not chat: the reranker's endpoints list names rerank
// alone, so the gateway refuses it, 400 endpoint_not_served, before any server
// (docs/specs/GATEWAY.md, Model metadata → Endpoints per model).
func (r *run) checkChatToReranker() {
	const name = "chat-to-reranker"
	resp, err := r.post(r.key, "/v1/chat/completions", idChatToReranker, r.chatBody(modelRerank, false, nil))
	if problem := r.notListed(resp, err, idChatToReranker, "/v1/chat/completions"); problem != "" {
		r.fail(name, "chat to %s: %s", modelRerank, problem)
		return
	}
	r.pass(name, "chat → 400 endpoint_not_served by the reranker's endpoints list, never sent")
}

// checkRerankToChat sends a rerank request to the chat model, whose server serves no
// rerank — vLLM's chat server has no rerank route, and llama-server started without
// --reranking answers 501: the chat model's endpoints list leaves rerank out, so the
// gateway refuses it, 400 endpoint_not_served, before any server.
func (r *run) checkRerankToChat() {
	const name = "rerank-to-chat"
	resp, err := r.post(r.key, "/v1/rerank", idRerankToChat, rerankBody(modelChat, relevanceDocuments))
	if problem := r.notListed(resp, err, idRerankToChat, "/v1/rerank"); problem != "" {
		r.fail(name, "rerank to %s: %s", modelChat, problem)
		return
	}
	r.pass(name, "rerank → 400 endpoint_not_served by the chat model's endpoints list, never sent")
}

// notListed checks the answer to request id for an endpoint at path the model's
// endpoints list leaves out: 400 endpoint_not_served with the list's message, and a
// log line showing it never reached a backend. "" when so, else what is wrong.
func (r *run) notListed(resp *response, err error, id, path string) string {
	if err != nil {
		return err.Error()
	}
	if problem := openAIError(resp, http.StatusBadRequest, "endpoint_not_served"); problem != "" {
		return problem
	}
	var e struct {
		Error struct{ Message string }
	}
	_ = json.Unmarshal(resp.body, &e)
	if want := "The model is not served on " + path + ": its configured endpoints do not include this API."; e.Error.Message != want {
		return fmt.Sprintf("message %q, want %q", e.Error.Message, want)
	}
	if notSent, err := r.notSent(id); err != nil || !notSent {
		return fmt.Sprintf("refused, but the log line shows it routed to a backend (%v)", err)
	}
	return ""
}
