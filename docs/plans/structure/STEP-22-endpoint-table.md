# Step 22 — one endpoint table

**Status:** not started

## Intent

What an endpoint is — route, metric name, GenAI operation, provider operation, error
shape, output-limit keys, whether it generates or counts — is one row in one table keyed
by `provider.Endpoint`. Today it is ~7 switches and ~6 equality checks over 6 server
files, joined to provider's own enum by `providerEndpoint`; adding an endpoint touches
~11 places in two packages.

## Findings

- server S3, provider F6 (independent F04): `endpointSpec{path, name, operation, api
  provider.Endpoint, body, counts, generates, outputLimitKeys, anthropicShape}`; the
  body endpoints *are* `provider.Endpoint` (the three model routes a small separate set);
  `providerEndpoint` and the switch functions (`path`, `name`, `operationName`,
  `counts`, `namesModel`, `errorShapeOf`, `outputLimitKeys`) become field reads;
  `errorShapeOf` and `answerModelEndpoint` share one "wants Anthropic" decision.
- server hint: the client route is the provider path with `/v1/` — state the path once.

## Files likely touched

- `gateway/internal/server/{pipeline,upstream,errors,params,inbound,inbound_messages,inbound_responses,models}.go`
  (and `attempts.go`/`relay.go` after step 10).
- `gateway/internal/provider/provider.go` (if the path is stated once there).

## Decisions made during planning

- Step 10's stage applicability reads the table's `body` field.
- Paths, metric labels, operation names and error shapes stay identical (no contract
  change).

## Removal checklist (clean at phase end)

- `git grep -nE 'func providerEndpoint|func \(e endpoint\) (path|name|operationName|counts|namesModel)|func errorShapeOf' gateway/` → none.

## Acceptance criteria

- Adding a body endpoint is one table row plus its format parser and the provider modules
  that serve it (show the list in the Result).
- `endpoints_test.go`, `api_test.go` and the request-line key test pass unchanged.
- `scripts/check-gateway.sh` green, or reds named with the step that clears them.
