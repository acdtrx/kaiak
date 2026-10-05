# Plan: pre-release audit fixes (2026-10-05)

## Goal

Fix what the 2026-10-05 review found before the next release
(`docs/reviews/2026-10-05/AUDIT.md`): H1, M1–M5, L1–L6, D1–D5. L7–L9 stay recorded in
the review.

## Scope

- **Credentials and remote text:** reserve `OTEL_` for the gateway (H1); keep remote
  bytes out of logs — collector answers (M2), provider transport errors and the
  control client's protocol diagnostics (M5).
- **Exporter delivery:** refuse redirects (M1); an unreadable `2xx` answer is a failure
  (M3); `Retry-After` edges (L1); a second stop signal cuts the final flush (L2), and
  the exit report is never rate-limited; recover from panicking values as slog does
  (L3).
- **Limits:** a short `Retry-After` when only in-flight reservations block a token
  limit (M4); `kaiak.limit.used` in dollars everywhere (L4).
- **Log vocabulary:** one key for a limit's group (L5); no status code for a response
  never sent (L6).
- **Docs:** D1–D5.

## Out of scope

L7 (lenient usage detail decoding), L8 (counting silent clamps), L9 (exporter byte
budget) — rare; recorded in the review.

## Decisions

Settled with the user (2026-10-05):

1. **The scope above.**
2. **L5:** `kaiak.limit.group` everywhere — the group's ID, absent for a global limit
   (`kaiak.limit.scope` says `global` or `group`); `kaiak.limit.id` is removed.
3. **L6:** `http.response.status_code` is absent when no response was sent;
   `error.type=client_closed` says what happened. The request-line metric and any
   status counting keep their own rule (checked in step 1).

Made while planning (confirm in review):

4. **H1:** `OTEL_` is reserved beside `KAIAK_` — the whole prefix, not a list of
   variable names, since endpoints can carry credentials too. Both schemas, both
   validators, the provider's last guard; a semantic rule or schema pattern, whichever
   `KAIAK_` uses today.
5. **M1:** the exporter refuses redirects (`http.ErrUseLastResponse`); a `3xx` fails the
   batch unretried and is reported (status only).
6. **M2/M5 rule:** an operational log line names the local failure class, the status,
   counts and our own IDs — never text or header values received from a remote party.
   Version mismatches report absent / invalid / the parsed number; control-plane error
   codes are logged only when they match the protocol's code shape.
7. **M3:** an empty `2xx` body is success; a read error, or a body that is not an
   `ExportLogsServiceResponse`, fails the batch unretried (some records may have been
   accepted; a retry could duplicate them).
8. **L1:** the retry wait is `max(Retry-After, backoff)`; a `Retry-After` larger than
   any representable or useful value saturates, so the deadline check fails the batch.
9. **M4:** minute windows track their in-flight reservations as hour windows do; when
   the request would fit once in-flight reservations settle, the refusal's
   `Retry-After` (and `x-ratelimit-reset-tokens`) is a short fixed value — 2 s — instead
   of the slot expiry or the window end. Hour windows the same.

## Constraints

- Zero third-party Go dependencies; no backwards compatibility (all unreleased).
- Codex's reproduction tests (`/tmp/kaiak/**/audit_b_test.go`,
  `/tmp/kaiak/control/kaiak-control/src/config/audit-b.test.ts`) are ported into the
  matching step as regression tests, renamed to the package's test naming; each must
  fail before its fix and pass after.

## Tag

Anchor `v0.9.2` on `main` right before step 1 (local only).

## Phases and steps

- **Phase 1 — audit fixes** (steps 1–5). Green at the end.
  1. `STEP-1-contract.md` — specs (`GATEWAY.md`, `CONTROL-PROTOCOL.md`), schemas and
     fixtures for H1, the delivery and log-content rules, M4's refusal rule, L5/L6 in
     the field tables; docs D1–D5.
  2. `STEP-2-exporter.md` — M1, M2, M3, L1, L2 (+ the unthrottled exit report), L3.
  3. `STEP-3-credentials-and-remote-text.md` — H1 (both halves), M5.
  4. `STEP-4-limits-and-vocabulary.md` — M4, L4, L5, L6.
  5. `STEP-5-verify.md` — the review's repros all pass; e2e for M1 and M4;
     `scripts/check-all.sh` ×3.

Expected reds inside the phase: after step 1, kaiak-control and the gateway fail the
new `OTEL_` fixtures (cleared by step 3); nothing else.

## Verification

- Every Codex repro, ported, fails on `v0.9.2` and passes at the end (recorded per step).
- The [X], [L] and [V] repros described in the review re-run as tests: redirect to
  another host and to a login page; `Retry-After: 0`; a panicking `Error()`; a refused
  agent admitted within the short `Retry-After`.
- `scripts/check-all.sh` green three times, Go test cache cleared before each.

**Verification status:** not started.
