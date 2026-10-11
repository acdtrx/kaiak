# Step 4 — uploads

**Status:** done — reviewed and committed 2026-10-11

## Intent

Teach the gateway to take `multipart/form-data` bodies on the endpoints that need them,
as a body kind beside JSON:
- read them under their own cap, memory budget and read deadline;
- parse them into parts under strict rules;
- send the backend a rebuilt body with `model` replaced.

No endpoint uses uploads until step 5, so this step is tested at the component level.

## Files likely touched

- `protocol/schema/config.schema.json`, `control/kaiak-control` (types, packaged
  schema, tests), `gateway/internal/config` (schema, snapshot, the effective cap):
  `global.max_upload_bytes`, default 25 MiB (OVERVIEW decision 17). Additive, so no
  version bump.
- `gateway/cmd/kaiak/` and the environment readers: `KAIAK_UPLOAD_MEMORY_BYTES`
  (default 256 MiB) and `KAIAK_UPLOAD_READ_TIMEOUT_MS` (default 300 000), with the
  same validation as their body counterparts (decisions 18–19).
- `gateway/internal/server/`:
  - `bodies.go`: a second budget for uploads (the `BodyBudget` type, another
    instance), taken step by step as today;
  - the read deadline for an upload route, extended from the listener's default before
    the body is read;
  - a multipart reader (a new file): the content type (`multipart/form-data` with a
    boundary, else refused — the code from step 1), the parts as slices of the held
    body (decision 28), the part rules (decision 21);
  - `pipeline.go`: an endpoint-table flag for upload endpoints, so `readInbound`
    takes the upload path;
  - the owned-parts hook each upload format fills in (steps 5–6).
- `gateway/internal/provider/`:
  - the rebuilt multipart body, written as it is sent, with its `Content-Length`;
  - `send.go`: the upstream `Content-Type` from the body kind (today always
    `application/json`);
  - `body.go`: the `model` replacement for multipart.
- `docs/specs/GATEWAY.md`, the errors table: the `unsupported_content_type` row, which
  lands with the code that answers it (`TestEveryErrorCodeHasItsClass` reads the
  table). Step 1 wrote it and the review moved it here:

  ```
  | An upload endpoint's body is not `multipart/form-data` with a `boundary` — OpenAI's JSON form of image edits included (settled 2026-10-11; Request pipeline → Uploads) | 400 | `invalid_request_error` | `unsupported_content_type` |
  ```
- Tests beside each.

## Decisions made during planning

- **The parser yields slices, never copies:** each part's headers and body point into
  the client's held body, so an upload costs its size once, in the upload budget.
  Whether that comes from a scanner written here or from `mime/multipart` with offset
  tracking is this step's call. **Differential tests** hold either to
  `mime/multipart`: every body the gateway accepts reads as the same parts there.
- **Strict part grammar:**
  - every part has exactly one `Content-Disposition: form-data` with a `name`;
  - a part with a `Content-Transfer-Encoding` header is refused (RFC 7578 deprecates
    it, and servers disagree on decoding it);
  - part headers are bounded (this step names the figure);
  - the body must end with the closing boundary; a preamble and an epilogue are
    ignored.
- **The rebuilt part headers** are `Content-Disposition` (name, and filename when
  present, quoted as `mime/multipart` quotes them) and `Content-Type` when the client
  sent one. Other part headers are dropped: RFC 7578 defines none, and the backend sees
  what the gateway parsed.
- **Single-value parts by name:** which names may repeat is per format (step 5,
  step 6); every other name is single-valued, and a repeat is refused like a duplicate
  JSON member (`400 duplicate_member`, `param` the part's name).
- **The upload cap is checked against `Content-Length` first,** then while reading,
  as the JSON cap is; the effective cap is the smaller of `max_upload_bytes` and the
  upload budget, with the same warning when the cap exceeds the budget.

## Acceptance criteria

- **Component tests:**
  - content-type refusals: none, JSON, multipart without a boundary;
  - parts: names, filenames, content types, empty parts, binary bodies containing
    boundary-like bytes;
  - refusals: a repeated single-value part, a part without a name, a
    `Content-Transfer-Encoding` part, oversize headers, an oversize text part
    (over 1 MiB), more than 1000 parts, a missing closing boundary;
  - differential tests against `mime/multipart`;
  - the rebuilt body: parts byte for byte, `model` replaced, a fresh boundary,
    `Content-Length` equal to the bytes written, nothing held beyond the client's body;
  - the cap, the upload budget (`503 server_busy` when full, the body budget
    untouched), the upload read deadline.
- `scripts/check-all.sh` run and recorded. Expected red: none.

## Result

### What changed

- **`global.max_upload_bytes`** (default 25 MiB), on both halves, additive (no version
  bump):
  - `protocol/schema/config.schema.json`: the field; `max_request_body_bytes`'
    description now says it caps JSON bodies, an upload having its own cap;
  - fixtures: `valid/full.json` sets 50 MiB, `valid/at-bounds.json` the bound; new
    `invalid/max-upload-bytes-zero.json` (schema) with its `cases.json` entry;
  - `control/kaiak-control`: `Global.max_upload_bytes` in the types; the packaged
    schema synced (`npm run sync-schemas`); the shared fixtures cover it;
  - `gateway/internal/config`: the walker, the document, `Snapshot.MaxUploadBytes`,
    `DefaultMaxUploadBytes`; the schema-defaults and the default/explicit tests.
- **`gateway/cmd/kaiak`**: `KAIAK_UPLOAD_MEMORY_BYTES` (whole bytes above 0, default
  256 MiB) and `KAIAK_UPLOAD_READ_TIMEOUT_MS` (whole ms above 0, default 300 000), read
  in the same loops as their body counterparts; `server.Uploads` wired into `NewAPI`;
  the config-apply warning "max_upload_bytes exceeds the upload budget…"
  (`kaiak.config.max_upload_size`, `kaiak.upload_budget.size`).
- **`gateway/internal/server`**:
  - `uploads.go` (new): `Uploads` (the upload budget, a second `BodyBudget`, and the
    read timeout), `DefaultUploadMemory`, `DefaultUploadReadTimeout`; the per-format
    hook `uploadFormat` (`lists`, the names that may repeat, and `read`, the format's
    owned parts past `model`, which also sets media and the input estimate);
    `readUpload` — content type first (before the body), the cap against
    `Content-Length`, the upload read deadline set in place of the body-read one,
    the body read under the cap and the upload budget step by step (`readBody`, as
    JSON bodies), the deadline cleared, the parts read, then `model` (a text part,
    not empty), then the format's `read`;
  - `form.go` (new): the multipart reader — `uploadBoundary` (the Content-Type) and
    `readForm` (the parts as slices of the held body);
  - `pipeline.go`: the endpoint row's `upload *uploadFormat` (non-nil marks an upload
    endpoint; none yet), `request.form`; `newPipeline`/`NewAPI` take `Uploads`;
  - `inbound.go`: `readInbound` takes the upload path for an upload row;
  - `listener.go`: `setBodyDeadline`; `bodies.go`: `releaseBody` drops the parts
    with the body; `attempts.go`: `provider.Request.Form`;
  - `errors.go`: `errUnsupportedContentType` (`unsupported_content_type`) and
    `errInvalidForm` (`invalid_body`, with a reason that never quotes the body).
- **`gateway/internal/provider`**:
  - `form.go` (new): `FormPart`; `formBody` — the rebuilt form (Content-Disposition
    with the name and, for a file part, the filename, quoted as mime/multipart's
    writer quotes them; the client's Content-Type when sent; `model`'s value the
    deployment's model; a fresh 30-random-byte boundary per attempt);
  - `provider.go`: `Request.Form` (nil for JSON);
  - `send.go`: `wireBody` picks the body kind — the edited JSON body, or the rebuilt
    form; the upstream `Content-Type` comes from it (`wireHeaders`); a form with a
    member edit or a pipeline parameter is a gateway fault;
  - `body.go`: `upstreamBody` sends pieces in order (the JSON body is one piece; a
    form is its made heads and the client's own part slices), size known up front;
    the reader moves through its own view of the shared pieces and drops them at the
    end, as before.
- **`docs/specs/GATEWAY.md`**: the `unsupported_content_type` errors-table row (the
  step's text); Request pipeline → Uploads: the boundary's rule, and the part
  grammar's figures and rules (below).

### Decisions made here

- **A scanner over the held body, not `mime/multipart`:** parts are slices of the
  client's bytes, and the scanner is stricter than mime/multipart everywhere it could
  read a body otherwise. Its headers' parameters are decoded by `mime.ParseMediaType`,
  the function mime/multipart uses, so names and filenames decode identically.
- **Strictness, each refused `400 invalid_body`** (written into the spec's Uploads →
  Parts):
  - a part's header block at most **8 KiB**, its blank line included;
  - each header one line — a folded header is refused; header names are tokens, values
    visible characters, spaces and tabs; at most one `Content-Type`;
  - lines end in CRLF; a boundary line ends at its boundary — **no transport
    padding** (HTTP clients send none; mime/multipart takes it, and other readers
    may not — so it is refused); a delimiter followed by a lone CR or LF, or by
    nothing, is refused too;
  - a **preamble of at most 4 KiB**, ending in CRLF, and the first line starting with
    the boundary must be the boundary line — mime/multipart reads a preamble line by
    line in a 4 KiB buffer, so the bound keeps every body the gateway takes readable
    there; the epilogue is unbounded and never read;
  - a name or filename holding an ASCII control character (possible through RFC 2231
    `name*=` decoding) is refused: the rebuilt header could not carry it as read;
  - a form with no part (`--b--` alone) is refused.
- **The boundary** must be RFC 2046's — 1 to 70 of its characters, not ending in a
  space — else `400 unsupported_content_type`: mime/multipart's reader assumes at most
  70, and every client checked sends one within it.
- **A file part** is one whose Content-Disposition names a `filename`, even empty (as
  Starlette reads it); the rebuilt body keeps an empty filename.
- **`model`** is read generically on every upload: absent or empty `400
  missing_required_parameter`, a file part `400 invalid_type`, a repeat `400
  duplicate_member` — all `param: "model"`.
- **The hook** is a field on the endpoint row, `upload *uploadFormat{lists, read}`, so
  a test row can carry its own format; steps 5–6 fill one in per format.
- **The input estimate** of an upload is the format's `read` to set (none yet).
- **The upload read deadline** is set only for a request declaring a body
  (`Content-Length` not 0), as the listener sets its own.

### Tests

- `server/form_test.go`: parts (names, filenames quoted, escaped and RFC 2231-encoded,
  content types present, absent and empty, empty parts in both shapes, every byte
  value and boundary-like bytes, a preamble and an epilogue, header names in any
  case), each part a slice of the body; the bounds at their edge (8 KiB headers, 1 MiB
  text part, a 2 MiB file part, 1000 parts, a 4 KiB preamble — and one more of each
  refused); 37 refusals; the Content-Type's boundary; **differential tests** — every
  accepted body compared with mime/multipart's reading: the fixed cases, 20 000 random
  forms (clean ones must be taken as written; about 3 000 messy ones are taken and
  compared), and `FuzzFormReadsAsMimeMultipart` (its seeds run with the tests; run with
  `-fuzz` for 90 s, 13.8 M inputs, no mismatch).
- `server/uploads_test.go` (a test-only upload row): the content-type refusals before
  the body is read; an upload read into its parts, held in the upload budget only and
  under the upload cap, not the JSON one, given back at the end; the model part's
  refusals; the cap (declared, budget-bounded, unknown length); the upload budget
  (`503 server_busy`, `Retry-After: 1`, the body budget untouched; chat served with the
  upload budget spent); the upload read deadline on a real listener, both longer and
  shorter than the body-read one.
- `provider/form_test.go`: the rebuilt body byte for byte against mime/multipart's
  writer, read back part by part, `model` replaced, a fresh boundary per build, the
  size equal to the bytes sent by the reader and by `GetBody`; the parts' bytes are
  the client's own slices; sent with an exact `Content-Length`, never chunked, a new
  boundary per attempt; a member edit on a form refused as a gateway fault; the client's
  upload unreachable once `Send` returned.
- `cmd/kaiak`: both settings' defaults, values and refusals; the upload-cap warning.
- `server/metrics_test.go`: the new code and constructors in the error-class check.

### Suite

`scripts/check-all.sh` — **all checks passed** (no expected reds):
- gateway: gofmt, vet, staticcheck 2026.2.1, the telemetry boundary, `go test -race
  -count=1 ./...` — every package `ok`, `e2e` in 125 s;
- live-test kit: lint and self-test passed for every kind;
- control: `npm test` 673 tests, 672 pass, 0 fail, 1 skipped (the store contract's
  reconnect test, as before; one more test than step 3: the new invalid fixture);
  `npm run lint` — `tsc` clean, "boundaries ok";
- cross-half e2e: `ok` in 76 s.
