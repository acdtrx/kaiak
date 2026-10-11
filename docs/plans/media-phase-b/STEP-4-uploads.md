# Step 4 — uploads

**Status:** not started

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
