# Step 26 — GUIDE.md trimmed

**Status:** done (2026-10-08)

## Intent

`control/kaiak-control/GUIDE.md` says what only it says (decision 3), so a store-contract
or pricing change stops editing it. Today it restates three contracts owned elsewhere,
opens with a dated change history, and is the most-changed doc.

## Findings

- control-core F6:
  - **keep:** division of labour, hard rules, the wiring, database-specific store advice
    (the SQL sketch, representation notes, restore steps, pitfalls), the ledger, the
    LiteLLM import mapping, operating, testing, "things that look wrong";
  - **per-method store contract:** point to `storage/types.ts` (its comments are
    complete) and the contract tests;
  - **backend types and prices:** one paragraph of what a UI must know, plus links;
  - **stream ordering:** one paragraph and a pointer (the rules live in
    `CONTROL-PROTOCOL.md` and the Fastify adapter);
  - **preamble:** one line naming the config format and protocol versions it matches.
- Everything earlier steps changed lands here once: `readTotals` for spend (step 8),
  window starts by limit type and the SQL join (step 13), `lastBatches` (step 24), the
  sweep's lifecycle (step 24).

## Files likely touched

- `control/kaiak-control/GUIDE.md`.

## Decisions made during planning

- `docs/specs/` does not ship in the package: where an app developer needs a contract to
  build against, the GUIDE keeps a short statement and names the shipped file (or the
  spec section) that owns it.

## Acceptance criteria

- No section restates a per-method store contract, a pricing rule or a stream-ordering
  rule beyond the one paragraph each; every link resolves.
- Every name the GUIDE mentions exists in the code (grep each exported name it uses).
- `npm test` green (the package's doc tests, if any).

## Result

**Size**: 719 → 702 lines; 50.6 KB → 44.8 KB, 7 549 → 6 697 words. The line count
drops little because the cut §5 table was ten very long lines, and its database advice
now sits as a list of wrapped lines.

**Cut, and where each contract now lives**

- Preamble: the dated change history → one line, "matches config format 5 and
  protocol version 5" (`format_version` const in `schema/config.schema.json`,
  `PROTOCOL_VERSION` in `src/protocol/index.ts`). A new paragraph says which paths are
  the package's and that spec references name sections, since `docs/specs/` does not
  ship.
- §4, the stream-ordering bullet → one bullet "Serving streams without the plugin":
  the rules are `CONTROL-PROTOCOL.md` → Config stream, implemented in `src/fastify/`;
  it names the core's reads (`readConfig`, `onConfigRead`, `readTotals`,
  `onDeliveryFailed`). The ordering and first-totals rules are no longer restated.
- §5, the per-method table → three pointers: the contract is `src/storage/types.ts`
  (comments per method and type), the cross-process guarantees are
  `CONTROL-PROTOCOL.md` → Control-plane processes, the check is
  `kaiak-control/store-contract`. Checked row by row against `types.ts`; every
  contract point is there. The table's database-specific advice stays, as a "Per
  method" list: one cursor per (instance, epoch) and why; `totalsSnapshot` in one
  `REPEATABLE READ` / one statement, and the classic split-read mistake; revisions
  from a sequence, gaps fine; an insertion-order column for `recentRecords`; past
  windows may be kept for reports; the change channel (LISTEN/NOTIFY, Redis), the
  catch-up on reconnect, no polling.
- §7, backend types (≈30 lines) → one list, "Backends and models: what a UI must
  know": `BACKEND_TYPES`, choosing a type, the type decides the client APIs,
  `api_key_env`, `base_url` help text from `schema/config.schema.json`'s
  descriptions, `metadata`/`MODEL_CAPABILITIES`/`output_limit`, no request defaults.
  Rules → `CONTROL-PROTOCOL.md` → Config → Backend types; `GATEWAY.md` → Providers,
  Model metadata. The base URL examples per type and the endpoint-by-type matrix
  are gone (the shipped schema's `type`/`base_url` descriptions carry the former).
- §7, prices (≈35 lines) → one list, "Prices: what a UI must know": dated entries,
  free without prices, tiers and the four units, the cache-unit fallback (one
  clause, which the LiteLLM mapping depends on), standard-tier rates, Azure
  deployment types (kept by name: `BACKLOG.md` cites it). Rules →
  `CONTROL-PROTOCOL.md` → Config → Prices, Tiered prices, Units and price units;
  `GATEWAY.md` → Providers → Service tier, Standard price on Anthropic types. The
  service-tier and Anthropic refusal details are gone.
- §7, group tree: the counter-memory figures (1.4 KB, 70 MB) are cut; the spec
  owns them. The rest stays (UI advice: defaults multiply, ID reuse, child_defaults
  not a ceiling).

**Kept, and why** (what only the GUIDE says): §1 division of labour, §2 hard rules,
§3 getting the package, §4 the minimal app and options, §5 the easy-to-miss database
points (primary reads, statement timeout, clocks), representation notes, the SQL
sketch, the LISTEN connection, restore steps (now headed "Restoring the store", the
name `DEPLOYMENT.md` cites), §6 the ledger, the LiteLLM import mapping (reflowed into
a list, content unchanged), the UI edit flow and keys, §8 verify (app-side usage of
`verifyBackend`), §9 reading state, §10 operating, §11 testing, §12 things that look
wrong. Section numbers are unchanged: `README.md`, `DEPLOYMENT.md`, `BACKLOG.md` and
`CONTROL-PROTOCOL.md` cite them.

**Kept in the GUIDE, missing from `types.ts`**: "gateway records returned must not
share mutable state with what callers hold (the memory store `structuredClone`s)" —
no `types.ts` comment says it; it stays as its own representation note ("Return
copies").

**Stale names and links fixed**

- §6: "Usage intake → Batch cursor retention" → `CONTROL-PROTOCOL.md` → **Status
  intake** → Batch cursor retention (the bullet is under Status intake). The same
  stale reference is in a code comment, `src/storage/types.ts:193`
  (`saveCountedBatch`); not touched (docs-only step) — a one-word comment fix for a
  later step.
- §6: `protocol/schema/usage-record.schema.json` → `schema/usage-record.schema.json`
  (the shipped copy).
- §10: the sample's `src/logging/` → `control/sample/src/logging/` (read as a package
  path under the new path rule).
- §11: the test files (`negative-control.test.ts`, `src/**/**.test.ts`) are named by
  their repo path, `control/kaiak-control/src/…`, with "the package does not ship
  them" (`files` excludes `src/**/*.test.ts`).
- §7/§8: `docs/specs/BACKEND-VERIFY.md` and "listed in the spec" → spec name and
  section (`CONTROL-PROTOCOL.md` → Config → Semantic rules).
- Steps 8, 13, 24, 25 had already brought the names in: `readTotals`,
  `windowStarts`, `scopeTypeKey`, `isCountedType`, `lastBatches`,
  `MODEL_CAPABILITIES`, `onExpirySweep` on the core; nothing further was stale.

**Name and link check**

- Every backticked identifier in the GUIDE was grepped in
  `control/kaiak-control/{src,schema}` and `control/sample/src` (tests excluded): all
  kaiak names found. The only misses are external names (SQL keywords, LiteLLM
  fields, `ERR_UNSUPPORTED_NODE_MODULES_TYPE_STRIPPING`, `node_modules`) and
  `crosshalf` (a Go build tag in `gateway/e2e`) and `private` (`package.json`).
- Every path resolves: `src/…`/`schema/…` against the package, the rest against the
  repo. Every spec reference names an existing section or bullet (Config stream;
  Control-plane processes; Status intake → Batch cursor retention; Config → Backend
  types, The group tree, Prices, Tiered prices, Units and price units, Semantic
  rules; `GATEWAY.md` → Providers (Endpoint support, Base URLs, Service tier,
  Standard price on Anthropic types), Model metadata).
- No test reads the GUIDE (`git grep GUIDE control/` finds only `package.json`'s
  `files`).

**Suite** (from `control/`)

```
npm test:      tests 629, pass 628, fail 0, skipped 1
npm run lint:  tsc -p . && node scripts/check-boundaries.ts — boundaries ok
```
