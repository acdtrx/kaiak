# Step 3 — the sample control plane

**Status:** done (2026-09-27)

## Intent

The sample runs on the new kit: keygen mints keys for a group, the status page shows
the tree and the totals per group.

## Files likely touched

- `control/sample/src/keygen/` and `keygen-cli.ts` — `--group <id>` replaces
  `--workload` / `--user`; the printed `keys` entry is `{ hash, group }`.
- `control/sample/src/page/sections.ts` (and `format.ts` if needed) — the config
  section lists groups as a tree (indentation by depth), each with its labels,
  allowed models and key IDs; totals vs limits per group path; recent usage shows
  the record's path.
- `control/sample/src/**/*.test.ts` and any sample config fixtures.
- `README.md` keygen line and `AGENTS.md` Project Facts keygen command (the command
  line changes).

## Decisions for this step

- The page reads labels only for display (`key=value` chips); it gives no label a
  meaning.

## Acceptance criteria

- `npm test` and `npm run lint` in `control/` fully green.
- The page renders a 4-level example config (manual check in a browser, screenshot
  deleted after) and escapes labels like any other config string.

## Result

**What changed** (`control/sample/src/`, plus two command lines)

- `keygen/index.ts` — `--group <id>` (required) replaces `--workload` / `--user`;
  the printed entry is `"<id>": { "hash": …, "group": … }`; `KeyOwner` gone;
  usage text `--id <key-id> --group <group-id>`. `--user` / `--workload` are now
  unknown options (refused by `parseArgs`).
- `page/sections.ts` — config section: counts name groups (no teams / workloads /
  users); Keys list shows `→ group <id>`; a new **Groups** block renders the tree
  as nested `<ul class="tree">` lists (top-level groups at the outer level, children
  under their parent, siblings in config order), built from `resolveScopes` paths.
  Each group line: ID, labels as `<span class="tag label">key=value</span>` chips,
  effective models (`all models` when unrestricted, `no models` tag for an empty
  list, else the resolved list), key IDs (or `none`). Totals: one row per limit of
  each `resolveScopes` entry, labelled `global` or the group path (`research /
  rag / rag-prod`, ancestors muted); windows matched by `(group or null,
  limitIdentity)`. Recent usage: `Owner` column → `Group`, the record's `groups`
  path in the same form.
- `page/document.ts` — two CSS rules (`.tree li`, `.label`).
- `app/index.ts` — unchanged: the carry-over log passes the `LimitCarryOver`
  object whole, so it logs the new `group` field as is.
- Tests: `keygen.test.ts` (group entry; the paste-in test uses
  `protocol/fixtures/config/valid/full.json` — `examples/` is format 1 until
  step 5; `--user` / `--workload` refused), `page.test.ts` (protocol 2, `group`
  windows, `key-group-unknown`, `mixed-groups.json`, new test "config: groups as a
  tree, each with its labels, effective models and keys", a label key and value
  escaped), `events.test.ts` (`mixed-groups.json`), `app.test.ts` /
  `main.test.ts` (`kaiak-protocol: 2`), `config-file.test.ts` (the invalid
  document is `{ format_version: 2 }`).
- `README.md` (three keygen lines → `--group`) and `AGENTS.md` Project Facts
  keygen command → `--id <key-id> --group <id>`. README's quick-start line uses
  `--group demo-app` with `examples/local-config.json`: step 5's rewrite of that
  example must keep a `demo-app` group.

**Decisions made during the step**

- Tree by nested lists, not indentation styles: the CSP allows only the page's own
  inline style block, and nested `<ul>` indents by depth with no per-element style.
- The tree's nesting comes from the resolver's `path`, not a second walk of
  `parent`: the page shows the same tree limits and models are resolved on.
- The Keys list stays (it carries `disabled` / `expires`); the tree repeats key IDs
  per group as the step asks.
- Group paths are written `a / b / c` with the group itself as the ID — one form
  for totals and usage.
- `child_defaults` are not listed per group: their effect is visible in each
  child's resolved models and in its limit rows under Totals.

**Manual check** — sample started from `control/` (`npm run dev -w sample`,
`KAIAK_SAMPLE_LISTEN=127.0.0.1:18090`) on a scratch config: `research` → `rag` →
`rag-prod` → `rag-api` (labels `kind=team|project|env|workload`, `research` also
`note=<b>&"`) plus `users` with `child_defaults` and child `alice`. `curl /`:
the tree nests four levels deep, `rag-api` shows `qwen3-32b` (intersection),
`alice` shows the default `bge-m3`, the label renders as
`note=&lt;b&gt;&amp;&quot;` (no raw `<b>&` anywhere in the page); totals rows
`global`, `research`, `research / rag / rag-prod`, `users / alice`. The first
start used a config missing model `metadata` and showed the rejection on the page;
the fixed file was picked up live. Server stopped, scratch files deleted.

**Suite** (from `control/`)

- `npm test`: `# tests 504 · pass 502 · fail 2` — the kit's `example configs` ›
  `config.json` and `local-config.json` (`examples/` still format 1). Expected;
  cleared by step 5. Every sample test green.
- `npm run lint`: `tsc` clean, `boundaries ok`.
