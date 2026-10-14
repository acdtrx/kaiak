# Step 11 — review and green

**Status:** done 2026-10-14 — three reviews triaged and fixed (below), the suite green
three times in a row on the final code (Runs), the `/tmp` copies deleted.

## Intent

Review the whole branch before the live run, fix what the user rules in, and leave
the suite green three times in a row.

## How

- **Two agent reviews of the worktree:** one on correctness, privacy and memory (who
  is captured, nothing sensitive reaching logs or metrics, the budget given back on
  every path, the tee, the finisher, the exit), one on spec conformance and tests
  (the conventions at the pinned commit, the OTLP JSON rules, the message mapping
  per API).
- **An independent review** of a plain copy (`/tmp/kaiak`, no git, no plan or review
  folders), with the diff since `v0.15.1` as `CHANGES.diff`: planned for Codex, run by
  a fresh agent that sees only the copy and the brief, as the user's Codex
  subscription had lapsed (2026-10-14).
- **Triage:** every finding checked against the code and reproduced; fixed with a test
  when it affects legitimate use or is high-impact; design-level findings put to the
  user; the rest recorded here.

## Acceptance criteria

- A table of findings: source, verdict, outcome.
- `scripts/check-all.sh` green **3× in a row** on the final code, recorded.
- The `/tmp` copies deleted, with no Codex processes left in them.

## Findings

Reviewer A: correctness, privacy and memory. Reviewer B: spec conformance and tests.
Reviewer C: the independent review of the plain copy.
The verdicts are the main session's, 2026-10-14. Every fix has a test that fails
without it (checked by reverting the fix), and the spec states it, dated 2026-10-14.

| Source | Finding | Verdict | Outcome |
|---|---|---|---|
| A | A request body over the export limit is still copied (up to the capture budget when an operator raises `max_request_body_bytes`), held, and built at about 7× its size live before the build drops it `too_large` — enough to OOM a pod | Fix: the request's content is carried nearly whole, so such a body cannot make a span under the limit | Fixed: `Start` drops it `too_large` at once — nothing copied, held or built; counted as `too_large`; the request line says `too_large`. The exporter's limit is read once, at `New`. Tests: `capture` `TestBodyOverTheExportLimitIsNotCopied`, `server` `TestCaptureBodyOverTheExportLimit`. Spec: Memory, The export limit, On the request line, the request line's `kaiak.capture`, the span processor's counts; `DEPLOYMENT.md` Resources and Request capture; decision 27 amended |
| A | A finisher that panics skips every later one — the capture's hand-off (its budget then leaks for good), the request line | Fix | Fixed: `finish` runs every finisher, then raises the first panic again for net/http. Test: `server` `TestFinisherPanicRunsTheOthers` (a panicking finisher run before the capture's: the capture is exported, `InUse()` 0, the line written, the panic re-raised) |
| A | Each queued capture's `job` (a few hundred bytes) is held outside the capture budget | Not worth it: a few hundred bytes per queued capture, while each holds at least its body in the budget | Not fixed |
| B | Tool definitions other than functions may have no `name`, which the conventions require (Langfuse skips them) | Fix | Fixed: such a tool keeps its JSON with a top-level `name` added — Chat's `custom.name`, else its type (Responses `local_shell`, `shell`, `apply_patch`); one whose `name` is not a string is left out. Tests: `TestGenericTool`, the corpus' unknown tools, each API's malformed tools |
| B | A finish reason expected and not received is `""` or left out; the conventions ask for `error`. A failed response's empty output message is made up output | Fix | Fixed: `error` at that position (a stream cut short or without its last event, a choice that never finished, a failed Responses answer); an output message with no parts is left out of a request whose span has an `error.type`, its finish reason kept. Tests: `TestFinishReasons`, `TestNoOutputMadeUpForAFailedRequest`, `TestFailedCaptureMakesUpNoOutput`, the cut-short, malformed and corpus cases, the server and e2e cut streams; the live-test kit's capture check mirrors it |
| B | The span status repeats `error.type` as its description; Recording Errors advises against it | Fix | Fixed: status `ERROR` with no message (`otlptrace.Status` has no message). Tests: `TestSpanAttributes`, `TestEncodeSpan`, the server and e2e span checks |
| B | A streamed Chat custom tool call is left out, where the same answer unstreamed carries it (a named exception to the twin rule) | Fix | Fixed: rebuilt from its fragments into the object the unstreamed answer carries; the exception removed from the spec. Tests: corpus case `a custom tool's call, OpenAI`, a cut custom call, `a custom call's fragments` |
| B | The Responses `file_id` mappings are unreachable: the inbound stage refuses a file ID before capture | Fix: remove | Fixed: the branches and their corpus cases removed; Chat's `file.file_id` stays (not refused). The spec says file IDs are refused before capture. Test of the premise: `TestCaptureStageDecides` (a Responses file ID and a Messages file source: refused, never captured) |
| B | Nothing in the tests holds captures to the conventions' schemas | Fix | Fixed: `expectWellFormed` checks roles, parts and their required members, tool definitions and one finish reason per output message; used by `expectCorpus` and the three fuzz targets (25 s each, green) |
| B | The session reason says "over 256" where the code measures bytes | Fix (spec only) | Fixed: "over 256 bytes" |
| B | OpenAI audio answers (`message.audio`) are not read | The spec allows leaving them out; kaiak serves no chat audio models | Not fixed |
| This step | Found by the conformance check: a part, block or item the mapping does not know whose `type` is one of the conventions' part types (`text`, `blob`, `tool_call`, …) is carried as a generic part a reader would take for that part, without its members | Fix | Fixed: left out (`appendGeneric`). Tests: `TestGenericPartNamedLikeTheConventions`, each API's malformed cases |
| C (medium) | A body of many tiny JSON values (`"stop":["",…]`, `"messages":[0,…]`, `"content":[{},…]`) makes one build hold 30–40× the body — 151 MiB for 4 MiB, up to ~590 MiB at 15 MiB — past the ~10× the spec promises and the sizing reserves; any key of a capturing group can send it, naming a model it may not use (refused, still captured) | Fix: the user chose a cap on the request's values over leaner reading or docs alone | Fixed (`e2b4791`): the builder counts the body's array elements and object members first — one pass, nothing allocated, an upper bound — and drops a request over 250,000 as `too_large` (counts alone) before reading it. Measured after: the old shapes dropped in under 1 ms; worst at the cap 42 MiB (26.6 MiB over 10× the body); the scan costs ~0.3 ms per MiB. Tests: `TestCountValues`, `TestRequestValueCap`, `TestBuildMemoryAtTheValueCap` (fails without the cap: 155 MiB against its 100 MiB bound). Spec: Memory, The request's values, the counts; `DEPLOYMENT.md` Resources and Request capture; decision 27 amended |
| C (low) | A final flush whose bound has already ended starts an export about half the time (select picks at random), its spans counted `timeout` rather than `shutdown` | Fix | Fixed (`aa51179`): `otlp.Sender.ForceFlush` returns the context's error at once when it has ended — for logs and traces, which share the sender. Test: `TestSenderForceFlushPastItsContextStartsNoSend` (100 trials; failed 3 of 3 runs without the fix) |
| C (low) | A Responses stream cut short runs the parts of an in-progress item together ("First step.Second step." as one reasoning part) | Fix | Fixed (`aa51179`): an in-progress item keeps its parts apart by the deltas' `content_index` / `summary_index`; deltas with none (llama-server's) build one part of their kind. Test: `TestResponsesCutShortKeepsEachPart`. Spec: Streams → Responses |
| C | Checked and found sound: the budget given back once on every path; no races, the finisher and exit order; capture never refuses or alters a request; privacy; the OTLP JSON (an export request decoded with the official `protojson` decoder); the explicit switch; ~20M fuzz executions of the content mapping; protocol 8 on both halves | — | — |
| Live run (step 12) | Langfuse 4.58 takes the reasoning tokens out of the output, as it takes the cache tokens out of the input | Doc | `DEPLOYMENT.md` → Langfuse says so (`aa51179`) |


## Runs

- 2026-10-14, after the fixes above: `go test -race` on the touched packages green;
  `FuzzChatCompletionsContent`, `FuzzMessagesContent`, `FuzzResponsesContent` 25 s
  each, no failure; `scripts/check-gateway.sh` green (`gateway checks passed`);
  `scripts/check-all.sh` green once (`all checks passed`: control 679 tests, 0
  failed; cross-half e2e `ok`). The 3× green runs come after the independent review.
- 2026-10-14, the final code (`e2b4791`), `scripts/check-all.sh` three times in a row,
  each `all checks passed` (13:08–13:20 local): every Go package `ok` (race, uncached),
  `kaiak/e2e` 139.5 / 140.6 / 141.7 s; the live-test kit's self-test `49 passed, 0
  failed, 3 skipped` and `self-test passed for vllm, llama-server, openai,
  azure-openai, anthropic, azure-anthropic, vllm with two backends`; control `pass
  678, fail 0, skipped 1`; the cross-half e2e `ok` (82.9 / 83.6 / 82.9 s).
- `/tmp/kaiak` and `/tmp/kaiak-audit-prompt.md` deleted; no process had its working
  directory there (`lsof -d cwd`), and Codex never started (its login had expired).
