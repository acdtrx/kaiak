# Structure refactor audit

Reviewed the supplied final tree and `BRANCH.diff` against v0.11.1. Reconstructed
v0.11.1 in `/tmp/kaiak-v0111` by reversing the diff, and ran both reproduction
tests there as well. No production code was changed.

## Findings

### S1 — medium — A drain deadline hides a subsequent hurry signal

**Location:** `gateway/internal/control/client.go:408`; caller:
`gateway/cmd/kaiak/controlplane.go:112`.

`controlPlane.finish` combines the drain deadline and the hurry context into one
child context. `Client.Finish` then treats only `context.Canceled` as a hurry.
Once the child's deadline expires, its error remains `context.DeadlineExceeded`,
even if the parent is subsequently cancelled.

If the second stop signal arrives after that deadline but before the usage flush
returns, `Finish` sends the final status despite the hurry. That request uses a
fresh background context and can delay shutdown by another two seconds. The
previous `finishWithControlPlane` checked the separate hurry channel after the
flush, so it skipped the status in this ordering. This violates the second-signal
behavior in `docs/specs/GATEWAY.md:2845`.

**Verification:** added
`gateway/cmd/kaiak/controlplane_codex_test.go`,
`TestCodexHurryAfterDrainDeadlineSkipsFinalStatus`. The test uses an already-expired
deadline and cancels the parent synchronously when the flush logs completion,
before the status decision. A fake HTTP transport counts requests; no network or
timing-dependent sleep is involved.

- Final tree: **FAIL**, `sent 1 final status request(s) after hurry; want 0`.
- v0.11.1: **PASS**, replacing the new `cp.finish(...)` call with its old
  `finishWithControlPlane(..., hurry.Done())` equivalent.

**Fix sketch:** preserve the hurry context separately from the usage-flush
deadline. After flushing, check the original hurry context before sending the
final status. Do not infer hurry state from the child context's first error.

### S2 — low — Output-limit validation messages changed

**Location:** `gateway/internal/config/semantic.go:92` and `:96`.

The typed config refactor changes output-limit values from `float64` to `int64`
and changes their diagnostic formatting from `%v` to `%d`. For large values, the
same rejected config now produces different text:

```text
v0.11.1: default 3e+06 is above ceiling 2e+06
branch:  default 3000000 is above ceiling 2000000

v0.11.1: ceiling 2e+06 is above context_length 1e+06
branch:  ceiling 2000000 is above context_length 1000000
```

Acceptance, error codes and paths are unchanged. The regression is limited to
operator-visible rejection text, which the review's behavior-preservation
requirement explicitly includes; it is not one of the listed exceptions.

**Verification:** added
`gateway/internal/config/semantic_codex_test.go`,
`TestCodexOutputLimitValidationMessagePreserved`. It parses the shared minimal
config with context length 1,000,000, ceiling 2,000,000 and default 3,000,000, and
compares the complete rejection message with v0.11.1.

- Final tree: **FAIL**, both values are printed in decimal instead of the previous
  scientific notation.
- v0.11.1: **PASS**, with the identical test file.

**Fix sketch:** retain integer storage and comparisons, but preserve the previous
numeric formatting in these two messages. Alternatively, explicitly approve and
document this additional visible change.

## Checks and results

- **Existing suites:** `go test ./...` passed before adding the reproductions.
  `go test -race ./... -skip TestCodex -count=1` passed all existing gateway
  packages, including e2e, without a reported race. `go vet ./...` passed.
- **Control:** `npm ci` succeeded; `npm test` reported **628 passed, 0 failed,
  1 skipped**. The existing skip is the reconnect/catch-up store-contract test:
  the memory store's channel never drops changes.
- **Limits:** inspected shared bases, counted generations, reservations, retained
  deleted-group counts, full versus changes-only totals, window rollover and
  per-minute shares. A temporary differential exercise of 100 deterministic
  operation sequences matched the baseline for ordinary current-window pushes,
  reloads and settlements. The exploratory test was removed.
- **Requests and providers:** checked attempt classification, retry refusal,
  cooldowns, circuit reporting, usage settlement, stream completeness and error
  events, body/header edits, output-limit handling and endpoint error shapes.
  Existing component and e2e checks passed. No additional confirmed regression.
- **Control lifecycle and storage:** checked the sweep relocation, listener
  helper, cursor API conversion, usage aggregation and sample feed shutdown.
  Compared public API removals with the “After 0.11.1” upgrade notes.
- **Contracts and tests:** checked the status schema change on both halves,
  protocol version, metric vocabulary, shared fixtures and test-helper
  consolidation. No third-party Go dependency was introduced. No unjustified
  assertion weakening was identified in the inspected changes; the existing
  finish test covers cancellation and deadline expiry separately, missing S1's
  combined ordering.

Reproduce the findings from `gateway/`:

```sh
GOCACHE=/tmp/kaiak-go-cache go test ./cmd/kaiak ./internal/config -run TestCodex -count=1 -v
```

```text
--- FAIL: TestCodexHurryAfterDrainDeadlineSkipsFinalStatus
    sent 1 final status request(s) after hurry; want 0
--- FAIL: TestCodexOutputLimitValidationMessagePreserved
    validation message changed
```

The two audit tests are intentionally left failing. They are the only retained
test additions.

## Not checked

- Real model servers, cloud credentials, deployed shutdown behavior and container
  images; validation used local fakes and in-process transports.
- The `crosshalf` build-tag suite; ordinary `go test ./...` does not include it.
- The complete `scripts/check-all.sh` toolchain, including staticcheck and Node
  lint. The commands above are the checks actually run.
- Race detection covers exercised executions, not every possible interleaving.
