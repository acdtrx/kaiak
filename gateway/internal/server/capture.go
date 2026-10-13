package server

import (
	"time"

	"kaiak/internal/capture"
	"kaiak/internal/clip"
)

// startCapture is the capture stage (docs/specs/GATEWAY.md, Request pipeline, stage
// 2; Request capture), run on the chat endpoints once the inbound stage has accepted
// the body. For a key whose own group captures, it starts the request's capture —
// the session header read, the body copied before the relay releases it, the client
// writer teeing into it — and registers the finisher that hands it to the builder.
// With trace export off (captures nil) it only marks the request line export_off.
// It never refuses, delays or alters the request: its own failures drop the
// capture, never the request.
func startCapture(rq *request, captures *capture.Captures) *apiError {
	if !rq.identity.Group.Capture {
		return nil
	}
	if captures == nil {
		rq.captureOutcome = capture.OutcomeExportOff
		return nil
	}
	c := captures.Start(rq.endpoint.api, rq.body, rq.r.Header.Values(capture.SessionHeader))
	rq.capture, rq.w.capture = c, c
	// Finishers run last registered first: this one runs after the attempts' and the
	// limits' — settlement — and before the request line, which names its outcome. It
	// only hands the capture on: finishers run before the response's end reaches the
	// client, and the span is built after it.
	rq.finishers = append(rq.finishers, func() { rq.captureOutcome = c.Hand(captureSummary(rq)) })
	return nil
}

// captureSummary is how rq ended, for its capture's span: what the request line
// reads, read the same way — copies of values the request holds, nothing computed
// that could wait for the builder.
func captureSummary(rq *request) capture.Summary {
	group := rq.identity.Group
	s := capture.Summary{Start: rq.start, End: time.Now(), RequestID: rq.id, KeyID: rq.keyID, Group: group.ID,
		RootGroup: group.PathIDs[0], Model: clip.String(rq.model), Stream: rq.inbound.Stream, Status: rq.w.status,
		ErrorType: errorCode(rq), RelayEnd: rq.relayEnd}
	if at := rq.answeringAttempt(); at.deployment.Backend != nil {
		s.Backend, s.DeploymentModel, s.Attempts = at.deployment.Backend, at.deployment.Model, len(rq.attempts)
	}
	if !rq.firstContent.IsZero() {
		s.ContentStreamed, s.TimeToFirstToken = true, rq.ttft
	}
	if u := rq.usage; u != nil {
		units, cost := rq.totalUsage()
		tokens := tokensOf(units)
		s.Usage = &capture.Usage{InputTokens: tokens.input, CacheReadTokens: tokens.cacheRead,
			CacheWriteTokens: tokens.cacheWrite, OutputTokens: tokens.output, ReasoningTokens: tokens.reasoning,
			CostUSD: costUSD(cost), Estimated: u.Estimated, Partial: u.Partial}
	}
	return s
}
