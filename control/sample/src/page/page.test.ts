// GET / renders every section from the control plane's state, escapes every value it
// interpolates, and carries no key material.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, test } from "node:test";

import Fastify from "fastify";
import type { Config, GatewayView, PublishedConfig, ReceivedRecord, Totals, UsageRecord } from "kaiak-control";

import type { ConfigFileState } from "../config-file/index.ts";

import { formatCount, formatNanoUsd, formatRelative, formatUrl } from "./format.ts";
import { escapeHtml, html, markupText } from "./html.ts";
import { registerStatusPage } from "./index.ts";
import type { StatusPageOptions } from "./index.ts";

const FIXTURES = path.resolve(import.meta.dirname, "../../../../protocol/fixtures");
const NOW = Date.parse("2026-09-24T10:30:00Z");
const EVIL = `<script>alert("x")</script>`;

function readFixture<T>(relative: string): T {
  return JSON.parse(readFileSync(path.join(FIXTURES, relative), "utf8")) as T;
}

function fixtureConfig(): Config {
  const config = readFixture<Config>("config/valid/full.json");
  // Values no valid config holds, to prove the page escapes whatever reaches it.
  const qwen = config.models["qwen3-32b"];
  assert.ok(qwen);
  config.models[EVIL] = qwen;
  config.keys[EVIL] = { hash: `sha256:${"a".repeat(64)}`, group: "alice" };
  const support = config.groups?.["support"];
  assert.ok(support);
  support.labels = { ...support.labels, [EVIL]: EVIL };
  return config;
}

function fixtureRecords(): ReceivedRecord[] {
  const batch = readFixture<{ records: UsageRecord[] }>("messages/usage-batch/valid/mixed-groups.json");
  const [workload, user] = batch.records;
  assert.ok(workload && user);
  return [
    // Written input of its own, so the cache-write column shows a value no other column does.
    { receivedAt: NOW - 5_000, record: { ...user, key_id: EVIL, units: { ...user.units, tokens_cache_write: 512 } } },
    { receivedAt: NOW - 65_000, record: workload },
  ];
}

// config_hash values: the current config's, an earlier config's, and one no config of
// this control plane has (a last-known-good copy from before it).
const CURRENT_HASH = "7".repeat(64);
const EARLIER_HASH = "6".repeat(64);
const OTHER_HASH = "f".repeat(64);

const GATEWAYS: GatewayView[] = [
  {
    instance: "gw-1",
    status: {
      instance: "gw-1",
      protocol_version: 5,
      state: "ready",
      started_at: "2026-09-24T09:58:12.5Z",
      applied_config_hash: CURRENT_HASH,
      last_rejection: null,
      backends: {
        "vllm-a": { in_flight: 3, max_in_flight: 4, deployments: { "Qwen/Qwen3-32B": { circuit: "closed" } } },
        "vllm-b": { in_flight: 0, deployments: { "Qwen/Qwen3-32B": { circuit: "open", opened_at: "2026-09-24T10:29:00Z" } } },
        "azure-westeurope": { in_flight: 1, deployments: { "gpt-4.1-prod": { circuit: "closed" } } },
        // Values no valid status holds, to prove the page escapes whatever reaches it.
        [EVIL]: { in_flight: 2, deployments: { [EVIL]: { circuit: "open", opened_at: "2026-09-24T10:00:00Z" } } },
      },
      models: { "qwen3-32b": { queued: 2 }, "gpt-4.1": { queued: 0 }, [EVIL]: { queued: 1 } },
    },
    receivedAt: NOW - 2_000,
    live: true,
  },
  {
    instance: "gw-2",
    status: {
      instance: "gw-2",
      protocol_version: 5,
      state: "draining",
      started_at: "2026-09-24T08:00:00Z",
      applied_config_hash: EARLIER_HASH,
      last_rejection: { config_hash: CURRENT_HASH, codes: ["key-group-unknown", EVIL] },
      backends: { "vllm-a": { in_flight: 0, deployments: {} } },
      models: {},
    },
    receivedAt: NOW - 45_000,
    live: false,
    conflict: { reason: "started-at-alternating", detectedAt: NOW - 50_000 },
  },
  {
    // A config this control plane does not hold (a last-known-good copy from before it).
    instance: "gw-3",
    status: {
      instance: "gw-3",
      protocol_version: 5,
      state: "ready",
      started_at: "2026-09-24T07:00:00Z",
      applied_config_hash: OTHER_HASH,
      last_rejection: null,
      backends: {
        "llama-embed": { in_flight: 0, deployments: { "/models/qwen3-embedding-0.6b-q8_0.gguf": { circuit: "half_open", opened_at: "2026-09-24T10:29:00Z" } } },
      },
      models: {},
    },
    receivedAt: NOW - 40_000,
    live: false,
  },
];

// Used amounts for three of the config's counted limits; the rest have used nothing.
const TOTALS: Totals = {
  live_gateways: 1,
  counted_through: [],
  windows: [
    { type: "usd_per_month", window_start: "2026-09-01T00:00:00Z", used: "1524000" },
    { group: "alice", type: "usd_per_month", window_start: "2026-09-01T00:00:00Z", used: "150000000000" },
    { group: "eval-pipeline", type: "tokens_per_hour", window_start: "2026-09-24T10:00:00Z", used: "1052" },
  ],
};

const FILE_STATE: ConfigFileState = {
  path: "/srv/kaiak/config.json",
  lastRun: undefined,
  lastFailure: {
    trigger: "file-changed",
    at: NOW - 10_000,
    ok: false,
    error: {
      code: "config-invalid",
      message: "1 issue",
      issues: [{ code: "key-group-unknown", path: "/keys/k-new/group", message: EVIL }],
    },
  },
};

function fakeCore(published: PublishedConfig | undefined): StatusPageOptions["controlPlane"] {
  const never = () => () => {};
  return {
    currentConfig: async () => published,
    gateways: async () => GATEWAYS,
    totals: async () => TOTALS,
    recentRecords: async () => (published ? fixtureRecords() : []),
    onConfigPublished: never,
    onTotalsChanged: never,
    onGatewaysChanged: never,
  };
}

const closers: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const close of closers.splice(0)) await close();
});

async function fetchPage(published: PublishedConfig | undefined, fileState: ConfigFileState = FILE_STATE) {
  const app = Fastify();
  closers.push(() => app.close());
  registerStatusPage(app, { controlPlane: fakeCore(published), configFile: () => fileState, clock: () => NOW });
  const response = await app.inject({ method: "GET", url: "/" });
  assert.equal(response.statusCode, 200);
  return response;
}

const PUBLISHED_CONFIG = fixtureConfig();
const PUBLISHED: PublishedConfig = {
  config: PUBLISHED_CONFIG,
  text: JSON.stringify(PUBLISHED_CONFIG),
  hash: CURRENT_HASH,
  publishedAt: NOW - 3_600_000,
};

// The part of the page between two section tags.
function section(page: string, id: string): string {
  const start = page.indexOf(`<section id="${id}">`);
  assert.notEqual(start, -1, `section ${id} present`);
  return page.slice(start, page.indexOf("</section>", start));
}

test("the page is HTML with a policy that allows only its own inline style and script", async () => {
  const response = await fetchPage(PUBLISHED);
  assert.match(String(response.headers["content-type"]), /^text\/html; charset=utf-8/);
  const policy = String(response.headers["content-security-policy"]);
  assert.match(policy, /default-src 'none'/);
  assert.match(policy, /script-src 'sha256-[A-Za-z0-9+/=]+'/);
  assert.doesNotMatch(policy, /unsafe-inline/);
  assert.match(response.body, /No authentication: this page is for local and demo use/);
});

test("gateways: state, live or expired, applied config or rejection by hash, serving summary, times, conflict", async () => {
  const gateways = section((await fetchPage(PUBLISHED)).body, "gateways");
  assert.match(gateways, /1 live of 3/);
  assert.match(
    gateways,
    /gw-1[\s\S]*ready[\s\S]*live[\s\S]*<code>777777777777<\/code><\/td>[\s\S]*6 in flight · <span class="tag warn">3 queued<\/span> · <span class="tag bad">2 circuits open<\/span>[\s\S]*2 s ago[\s\S]*2026-09-24 09:58:12 UTC/,
  );
  assert.match(
    gateways,
    /gw-2[\s\S]*draining[\s\S]*expired[\s\S]*<code>666666666666<\/code> <span class="tag warn">not current<\/span><br><span class="tag bad">rejected <code>777777777777<\/code><\/span> key-group-unknown[\s\S]*idle/,
  );
  assert.match(gateways, /conflict<\/span> started-at-alternating/);
  assert.match(gateways, /gw-3[\s\S]*<code>ffffffffffff<\/code> <span class="tag warn">not current<\/span>[\s\S]*<span class="tag warn">1 circuit half-open<\/span>/);
});

// The part of the gateways section detailing one gateway's backends and queues.
function routing(gateways: string, instance: string): string {
  const start = gateways.indexOf(`<h3>${instance} <span class="muted">backends and queues</span></h3>`);
  assert.notEqual(start, -1, `routing detail for ${instance} present`);
  const end = gateways.indexOf("<h3>", start + 1);
  return gateways.slice(start, end === -1 ? undefined : end);
}

test("gateways: per gateway, backends with in flight against the cap, circuits, and queued models", async () => {
  const gateways = section((await fetchPage(PUBLISHED)).body, "gateways");
  const gw1 = routing(gateways, "gw-1");
  // Capped and uncapped backends, sorted by ID.
  assert.match(gw1, /<td class="id">azure-westeurope<\/td>\s*<td class="num">1 \/ —<\/td>\s*<td><span class="id">gpt-4.1-prod<\/span><\/td>/);
  assert.match(gw1, /<td class="id">vllm-a<\/td>\s*<td class="num">3 \/ 4<\/td>\s*<td><span class="id">Qwen\/Qwen3-32B<\/span><\/td>/);
  assert.ok(gw1.indexOf(">azure-westeurope<") < gw1.indexOf(">vllm-a<") && gw1.indexOf(">vllm-a<") < gw1.indexOf(">vllm-b<"), "backends sorted");
  // An open circuit: flagged, with when it opened, relative and absolute.
  assert.match(
    gw1,
    /<td class="id">vllm-b<\/td>\s*<td class="num">0 \/ —<\/td>\s*<td><span class="id">Qwen\/Qwen3-32B<\/span> <span class="tag bad">circuit open<\/span> since <time datetime="2026-09-24T10:29:00.000Z" data-relative>1 min ago<\/time> <span class="muted">2026-09-24 10:29:00 UTC<\/span><\/td>/,
  );
  // Only models with requests waiting are listed.
  assert.match(gw1, /Queued: .*<span class="id">qwen3-32b<\/span> <span class="tag warn">2<\/span><\/p>/);
  assert.doesNotMatch(gw1, /gpt-4\.1<\/span> <span class="tag warn">/);
  const gw2 = routing(gateways, "gw-2");
  assert.match(gw2, /<td class="id">vllm-a<\/td>\s*<td class="num">0 \/ —<\/td>\s*<td><span class="muted">none in the applied config<\/span><\/td>/);
  assert.match(gw2, /Queued: <span class="muted">none<\/span>/);
  // A half-open circuit: flagged apart from an open one, with when it opened.
  assert.match(
    routing(gateways, "gw-3"),
    /<span class="id">\/models\/qwen3-embedding-0.6b-q8_0.gguf<\/span> <span class="tag warn">circuit half-open<\/span> since <time datetime="2026-09-24T10:29:00.000Z"/,
  );
});

test("config: its hash and publish time, the file's rejection with its issues, a summary without key hashes", async () => {
  const config = section((await fetchPage(PUBLISHED)).body, "config");
  assert.match(config, /<h2>Config <span class="muted"><code>777777777777<\/code><\/span><\/h2>/);
  assert.match(config, /published <time datetime="2026-09-24T09:30:00.000Z" data-relative>1 h ago<\/time>/);
  assert.match(config, /The latest edit was rejected \(config-invalid\)<\/strong> — file-changed/);
  assert.match(config, /Gateways keep the current config/);
  assert.match(config, /<code>\/keys\/k-new\/group<\/code> key-group-unknown/);
  assert.match(config, /3 backends · 5 models · 7 keys · 8 groups/);
  assert.match(config, /qwen3-32b<\/span> → vllm-a\/Qwen\/Qwen3-32B/);
  assert.match(config, /k-eval-ci<\/span> → group eval-pipeline/);
  assert.match(config, /k-bob-old<\/span> → group bob <span class="tag bad">disabled<\/span>/);
});

// The line of one group in the tree, up to its children's list or its end.
function groupItem(config: string, id: string): string {
  const start = config.indexOf(`<li><span class="id">${id}</span>`);
  assert.notEqual(start, -1, `group ${id} listed`);
  const rest = config.slice(start + 4);
  const end = Math.min(...["<ul", "</li>", "<li>"].map((tag) => rest.indexOf(tag)).filter((at) => at !== -1));
  return rest.slice(0, end);
}

test("config: groups as a tree, each with its labels, effective models and keys", async () => {
  const config = section((await fetchPage(PUBLISHED)).body, "config");
  const tree = config.slice(config.indexOf("<h3>Groups</h3>"));
  // Top-level groups at the outer level, each group's children nested under it.
  const nested = (parent: string, children: string[]): void =>
    assert.match(tree, new RegExp(`<li><span class="id">${parent}</span>[^]*?<ul class="tree">${children.map((child) => `<li><span class="id">${child}</span>[^]*?</li>`).join("")}</ul></li>`));
  nested("research", ["eval-pipeline"]);
  nested("support", ["support-bot"]);
  nested("users", ["alice", "bob", "carol"]);
  assert.match(tree, /^<h3>Groups<\/h3><ul class="tree"><li><span class="id">research<\/span>/);
  // Labels as key=value chips; models as resolved down the path.
  assert.match(groupItem(tree, "research"), /<span class="tag label">kind=team<\/span> <span class="tag label">owner=research-leads@example.com<\/span>\s*<span class="muted">models:<\/span> <span class="muted">all models<\/span> <span class="muted">keys:<\/span> <span class="muted">none<\/span>/);
  assert.match(groupItem(tree, "eval-pipeline"), /models:<\/span> <span class="muted">all models<\/span> <span class="muted">keys:<\/span> <span class="id">k-eval-ci<\/span>$/);
  assert.match(groupItem(tree, "support-bot"), /models:<\/span> bge-m3, gpt-4.1-mini /);
  // bob takes the users' default list and has two keys.
  assert.match(groupItem(tree, "bob"), /models:<\/span> bge-m3, gpt-4.1-mini, qwen3-32b <span class="muted">keys:<\/span> <span class="id">k-bob<\/span>, <span class="id">k-bob-old<\/span>$/);
  assert.match(groupItem(tree, "alice"), /all models/);
});

test("config: before any publish the page says so, and the startup rejection shows", async () => {
  const state: ConfigFileState = { ...FILE_STATE, lastFailure: { trigger: "startup", at: NOW, ok: false, error: { code: "json-invalid", message: "Unexpected token" } } };
  const page = (await fetchPage(undefined, state)).body;
  const config = section(page, "config");
  assert.match(config, /rejected \(json-invalid\)[\s\S]*Nothing is published: gateways wait for a config until the file is fixed/);
  assert.match(config, /No config published yet/);
  assert.match(section(page, "totals"), /No config published yet/);
  assert.match(section(page, "usage"), /No usage received yet/);
});

test("totals: every limit of every scope, used against its value, zero included; per-minute limits noted", async () => {
  const totals = section((await fetchPage(PUBLISHED)).body, "totals");
  const row = (pattern: RegExp): void => assert.match(totals, pattern);
  // Used: 1,524,000 nano-USD of $5,000.
  row(/<td>global<\/td><td>\$5,000.00 \/ month<\/td>\s*<td class="num">\$0.001524<\/td><td class="bar"><meter [^>]*value="0"><\/meter> <span class="num">0.0%<\/span><\/td><td>2026-09-01 00:00:00 UTC<\/td>/);
  // $150 of alice's $200 (her override of the default user's $20).
  row(/<td><span class="muted">users \/<\/span> <span class="id">alice<\/span><\/td><td>\$200.00 \/ month<\/td>[\s\S]*?\$150.00<\/td><td class="bar"><meter [^>]*value="75"><\/meter> <span class="num">75%/);
  row(/<td><span class="muted">research \/<\/span> <span class="id">eval-pipeline<\/span><\/td><td>20,000,000 tokens \/ hour<\/td>\s*<td class="num">1,052<\/td>[\s\S]*?2026-09-24 10:00:00 UTC/);
  // Nothing used yet: 0 of the limit, the current window.
  row(/<td><span class="id">research<\/span><\/td><td>\$1,500.00 \/ month<\/td>\s*<td class="num">\$0.00<\/td>/);
  row(/users \/<\/span> <span class="id">bob<\/span><\/td><td>2,000,000 tokens \/ hour<\/td>[\s\S]*?<td class="num">0<\/td>[\s\S]*?2026-09-24 10:00:00 UTC/);
  row(/<td>global<\/td><td>3,000 requests \/ min<\/td>[\s\S]*?per gateway share, not counted here/);
});

test("recent usage: one row per record, newest first, with the group path, tokens, cost and flags", async () => {
  const usage = section((await fetchPage(PUBLISHED)).body, "usage");
  assert.match(usage, /last 2/);
  const alice = usage.indexOf('users /</span> <span class="id">alice</span>');
  const workload = usage.indexOf("eval-pipeline");
  assert.ok(alice !== -1 && workload !== -1 && alice < workload, "newest first");
  assert.match(usage, /5 s ago[\s\S]*gw-1[\s\S]*<td><span class="muted">users \/<\/span> <span class="id">alice<\/span><\/td>[\s\S]*gpt-4.1-mini[\s\S]*azure-westeurope\/gpt-4.1-mini[\s\S]*2,048<\/td>\s*<td class="num">1,024<\/td>\s*<td class="num">512<\/td>\s*<td class="num">377<\/td>\s*<td class="num">0<\/td>\s*<td class="num">\$0.001524<\/td>[\s\S]*estimated[\s\S]*partial/);
  assert.match(usage, /k-eval-ci[\s\S]*<td><span class="muted">research \/<\/span> <span class="id">eval-pipeline<\/span><\/td>[\s\S]*812<\/td>/);
});

test("every interpolated value is escaped", async () => {
  const page = (await fetchPage(PUBLISHED)).body;
  // The only script element is the page's own.
  assert.equal(page.split("<script").length - 1, 1);
  const escaped = escapeHtml(EVIL);
  assert.equal(escaped, "&lt;script&gt;alert(&quot;x&quot;)&lt;/script&gt;");
  for (const id of ["gateways", "config", "usage"]) assert.ok(section(page, id).includes(escaped), `${id} shows the value escaped`);
  // A group label: key and value.
  assert.ok(section(page, "config").includes(`<span class="tag label">${escaped}=${escaped}</span>`), "label escaped");
  // A backend ID, a deployment model and a model name from a gateway's status.
  const gw1 = routing(section(page, "gateways"), "gw-1");
  assert.ok(gw1.includes(`<td class="id">${escaped}</td>`), "backend ID escaped");
  assert.ok(gw1.includes(`<span class="id">${escaped}</span> <span class="tag bad">circuit open</span>`), "deployment model escaped");
  assert.ok(gw1.includes(`<span class="id">${escaped}</span> <span class="tag warn">1</span>`), "queued model escaped");
});

test("no key material in the page: key IDs only, never hashes or keys", async () => {
  const page = (await fetchPage(PUBLISHED)).body;
  assert.doesNotMatch(page, /sha256:/);
  for (const key of Object.values(PUBLISHED.config.keys)) assert.ok(!page.includes(key.hash.slice(7)));
  assert.doesNotMatch(page, /kaiak-[A-Za-z0-9]{43}/);
});

test("the page says its state lives in memory and resets on restart: not a billing system", async () => {
  const page = (await fetchPage(PUBLISHED)).body;
  assert.match(
    page,
    /<p class="notice"><strong>State lives in memory\.<\/strong> Budgets, usage totals and the de-duplication of usage batches reset when this process restarts: [^<]*not a billing system\.<\/p>/,
  );
  assert.ok(page.indexOf('class="notice"') < page.indexOf("<main>"), "the notice leads the page");
});

test("backend URLs are shown without userinfo, whatever the config holds", async () => {
  const config = fixtureConfig();
  const backend = config.backends["vllm-a"];
  assert.ok(backend);
  // No valid config holds this: validation rejects userinfo. The page drops it anyway.
  config.backends["vllm-a"] = { ...backend, base_url: "http://operator:s3cret-pass@vllm-a.internal:8000/v1" };
  const page = (await fetchPage({ ...PUBLISHED, config })).body;
  assert.match(section(page, "config"), /<span class="id">vllm-a<\/span> openai-compatible <span class="muted">http:\/\/vllm-a\.internal:8000\/v1<\/span>/);
  assert.ok(!page.includes("s3cret-pass") && !page.includes("operator"), "no credential in the page");
  assert.equal(formatUrl("https://user@acme.openai.azure.com"), "https://acme.openai.azure.com/");
  assert.equal(formatUrl("not a url"), "(not a URL)");
});

test("html escapes text and nests markup; formats amounts exactly", () => {
  assert.equal(markupText(html`<b>${"<i>&'"}</b>${[html`<br>`, 1, 2n, null, undefined, false]}`), "<b>&lt;i&gt;&amp;&#39;</b><br>12");
  assert.equal(formatNanoUsd(0n), "$0.00");
  assert.equal(formatNanoUsd(1_524_000n), "$0.001524");
  assert.equal(formatNanoUsd(12_500_000_000n), "$12.50");
  assert.equal(formatNanoUsd(10n ** 18n - 1n), "$999,999,999.999999999");
  assert.equal(formatCount(1_234_567), "1,234,567");
  assert.equal(formatCount(12_345_678_901_234_567_890n), "12,345,678,901,234,567,890");
  assert.equal(formatRelative(NOW - 400, NOW), "just now");
  assert.equal(formatRelative(NOW - 59_000, NOW), "59 s ago");
  assert.equal(formatRelative(NOW - 3_599_000, NOW), "59 min ago");
  assert.equal(formatRelative(NOW - 90_000_000, NOW), "1 d ago");
});
