# Step 10 — sample live page

**Status:** done (2026-09-24)

## Intent

One read-only page showing the control plane's state, updated live.

## Files likely touched

- `control/sample/src/page/` — server-rendered HTML, a page SSE endpoint, a small
  inline script to apply updates; no build step, no framework, no CDN

## Decisions made during planning

- Sections: live gateways with status; applied config version per gateway or its
  latest rejection; totals vs limits per scope and window; last 100 usage records;
  the config file's error when an edit was rejected.
- Snapshot + subscribe: the page renders full state, then one SSE connection per
  browser pushes changes (CODING-RULES §7: pushed, not polled).
- Page auth: none for the sample (local and demo use) — stated on the page and in the
  README; it shows key IDs, never keys.

### Decisions made during implementation

- **Subsystem** `control/sample/src/page/`: `registerStatusPage(app, { controlPlane,
  configFile: () => state })` registers `GET /` and `GET /events` on the app and
  returns `{ configFileChanged }`, which `app` calls after every config file run (the
  core announces publishes; rejections and unchanged runs only the file sees — and a
  published run clears the failure after the core's publish listeners ran, so the
  run itself must also push).
- **Rendering**: template literals through an `html` tag that escapes every
  interpolated value unless it is markup made by `html`; there is no way to mark a
  plain string safe. The inline style and script are module constants outside any
  interpolation. A `Content-Security-Policy` allows exactly those two blocks by hash
  (`default-src 'none'`, `connect-src 'self'`); bars are native `<meter>` elements,
  so no inline style attributes are needed.
- **Sections**: gateways; config (version and publish time, the file's latest
  failure with trigger, time, code, message and issues; a summary — counts, models
  with deployments, backends, key IDs with owner/disabled/expiry, owners; never key
  hashes); totals vs limits (every limit of every scope from `resolveScopeLimits`,
  used from the totals or 0, current window start computed for unused ones;
  per-minute limits listed as "per gateway share, not counted here"); recent usage
  (the core's last 100 records). Limits appear once, in totals, not again in the
  config summary.
- **Live updates**: `event: section`, data `{"id","html"}` — the server re-renders a
  changed section and the inline script swaps its `innerHTML` (ids checked against
  the known four). Every connect, reconnects included, gets all four sections.
  Changes map to sections: publish → config, totals, gateways (the "behind vN"
  marker); counted batch → totals, usage; status → gateways (plus totals when the
  live count changes); config file run → config. **Coalescing is shared by all
  browsers**: a change pushes at once if the last push was over a second ago,
  otherwise one trailing push carries every section changed meanwhile, rendered
  once for everyone. Renders (connect and pushes) take turns so a browser never
  gets an older render after a newer one. The page subscribes to the core only
  while a browser is connected. Heartbeat comment every 15 s; a browser that has
  not read for 30 s is dropped (it reconnects and gets everything).
- **Relative times**: rendered server-side with the absolute time beside them;
  the inline script refreshes them every 5 s from `<time datetime>` (its rule
  mirrors `formatRelative`). Amounts: tokens with thousands separators, nano-USD as
  dollars exactly via bigint (2–9 decimals).
- **Kit entry**: `limitIdentity` and `resolveScopeLimits` are now value exports of
  `kaiak-control` (they were type-only re-exports) — any host showing limits
  against totals needs them, the real control plane's UI included.
- **No `docs/specs/SAMPLE.md`**: the page is not a contract; decisions live here and
  in the README.
- The totals are read with an empty instance name: the page ignores
  `counted_through`.

## Result

- `control/`: `npm test` — 354 tests, 354 pass (15 new: 9 render tests with an
  injected core — each section, escaping of a `<script>` model name, key ID, issue
  message and rejection code, no `sha256:`/key pattern, formatting; 5 stream tests
  over a real listening app and real core — all sections on connect and reconnect,
  pushes after status, publish, batch and config file rejection, a 6-status burst
  pushed once after the interval with the latest state, subscriptions released
  when the last browser leaves, `app.close()` ending the stream; 1 app test — the
  page and stream never hold the gateway token, a broken file edit is pushed live).
  `npm run lint` — `boundaries ok`.
- `scripts/check-gateway.sh` — `gateway checks passed`.
- Manual (scratch dir, deleted; ports 18000/18090/28080/29090 since 8000 and 18080
  were taken by processes outside this step): fake backend, the sample on a copy of
  `examples/local-config.json` with a keygen key, a built `kaiak` in control mode.
  `curl /` → 200 with the CSP, all four sections. `curl -N /events` got the four
  sections on connect; 5 requests (all 200) → gateways, totals (team tokens 280 of
  1,000,000), usage (5 rows, `k-demo`, `$0.000203` each); a limit added to the file →
  config v2, totals with the new `$2.50 / month` row at `$0.00`, gateways `v2`;
  `{ broken` → config with "rejected (json-invalid) … Gateways keep v2". No key, no
  token, no `sha256:` in the page or the stream. Browser check: the user's
  (OVERVIEW 3).
- Expected reds: none.

## Acceptance criteria

- Tests: page renders each section from store state; SSE pushes an update after a
  batch/status; no key material anywhere in the HTML.
- Manual with the user (OVERVIEW 3).
