// The page's sections, each rendered whole from the control plane's current state:
// the page on load, and every live update, is these renders. Everything interpolated
// goes through `html`, which escapes it.

import { resolveScopes } from "kaiak-control";
import type { Config, ControlPlane, DeploymentStatus, GatewayStatus, GatewayView, Limit, PublishedConfig, ReceivedRecord, ResolvedScope, Totals } from "kaiak-control";

import type { ConfigFileState } from "../config-file/index.ts";

import { formatAbsolute, formatCount, formatNanoUsd, formatUrl, parseTimestamp, timeAgo, usdToNano } from "./format.ts";
import { html } from "./html.ts";
import type { Markup } from "./html.ts";

export type SectionId = "gateways" | "config" | "totals" | "usage";

export const SECTION_IDS: readonly SectionId[] = ["gateways", "config", "totals", "usage"];

// What the sections read.
export interface PageSources {
  core: Pick<ControlPlane, "currentConfig" | "gateways" | "totals" | "recentRecords">;
  configFile: () => ConfigFileState;
  // Milliseconds since the epoch.
  clock: () => number;
}

// The inner markup of one section, heading included.
export function renderSection(id: SectionId, sources: PageSources): Promise<Markup> {
  switch (id) {
    case "gateways":
      return renderGateways(sources);
    case "config":
      return renderConfig(sources);
    case "totals":
      return renderTotals(sources);
    case "usage":
      return renderUsage(sources);
  }
}

// ---- Gateways

async function renderGateways({ core, clock }: PageSources): Promise<Markup> {
  const [gateways, current] = await Promise.all([core.gateways(), core.currentConfig()]);
  const now = clock();
  const live = gateways.filter((gateway) => gateway.live).length;
  const heading = html`<h2>Gateways <span class="muted">${live} live of ${gateways.length}</span></h2>`;
  if (gateways.length === 0) return html`${heading}<p class="muted">No gateway has reported yet.</p>`;
  return html`${heading}
<div class="scroll"><table>
<thead><tr><th>Instance</th><th>State</th><th>Live</th><th>Config</th><th>Serving</th><th>Last report</th><th>Started</th><th>Conflict</th></tr></thead>
<tbody>${gateways.map((gateway) => gatewayRow(gateway, current, now))}</tbody>
</table></div>
${gateways.map((gateway) => routingDetail(gateway, now))}`;
}

function gatewayRow(gateway: GatewayView, current: PublishedConfig | undefined, now: number): Markup {
  const { status } = gateway;
  const started = parseTimestamp(status.started_at);
  return html`<tr>
<td class="id">${gateway.instance}</td>
<td><span class="tag state-${status.state}">${status.state}</span></td>
<td>${gateway.live ? html`<span class="tag ok">live</span>` : html`<span class="tag warn">expired</span>`}</td>
<td>${appliedConfig(gateway, current)}</td>
<td>${servingSummary(status)}</td>
<td>${timeAgo(gateway.receivedAt, now)}</td>
<td>${started === undefined ? status.started_at : formatAbsolute(started)}</td>
<td>${gateway.conflict ? html`<span class="tag bad">conflict</span> ${gateway.conflict.reason} <span class="muted">(${timeAgo(gateway.conflict.detectedAt, now)})</span>` : html`<span class="muted">none</span>`}</td>
</tr>`;
}

// The gateway's applied config against the current one, by config_hash: a gateway
// running anything else (a config it was sent before, its last-known-good copy) is
// flagged as not current.
function appliedConfig({ status }: GatewayView, current: PublishedConfig | undefined): Markup {
  const applied = status.applied_config_hash;
  const shown =
    applied === null
      ? html`<span class="muted">none applied</span>`
      : html`<code>${shortHash(applied)}</code>${current !== undefined && applied !== current.hash && html` <span class="tag warn">not current</span>`}`;
  const rejection = status.last_rejection;
  if (!rejection) return shown;
  return html`${shown}<br><span class="tag bad">rejected <code>${shortHash(rejection.config_hash)}</code></span> ${rejection.codes.join(", ")}`;
}

// The first 12 hex digits of a config_hash: enough to tell configs apart on the page.
function shortHash(hash: string): string {
  return hash.slice(0, 12);
}

// One line per gateway: requests in flight and queued, circuits open and half-open;
// "idle" when all four are zero.
function servingSummary({ backends, models }: GatewayStatus): Markup {
  const backendList = Object.values(backends);
  const inFlight = sum(backendList.map((backend) => backend.in_flight));
  const queued = sum(Object.values(models).map((model) => model.queued));
  const circuits = (state: DeploymentStatus["circuit"]) =>
    sum(backendList.map((backend) => Object.values(backend.deployments).filter((deployment) => deployment.circuit === state).length));
  const open = circuits("open");
  const halfOpen = circuits("half_open");
  if (inFlight === 0 && queued === 0 && open === 0 && halfOpen === 0) return html`<span class="muted">idle</span>`;
  return html`${formatCount(inFlight)} in flight${queued > 0 && html` · <span class="tag warn">${formatCount(queued)} queued</span>`}${open > 0 && html` · <span class="tag bad">${counted(open, "circuit")} open</span>`}${halfOpen > 0 && html` · <span class="tag warn">${counted(halfOpen, "circuit")} half-open</span>`}`;
}

// Per gateway, as its latest status reports them: each backend's in-flight count
// against its cap and its deployments' circuits, then the models with requests
// queued.
function routingDetail({ instance, status }: GatewayView, now: number): Markup {
  const backends = sortedEntries(status.backends);
  const queued = sortedEntries(status.models).filter(([, model]) => model.queued > 0);
  return html`<h3>${instance} <span class="muted">backends and queues</span></h3>
${
  backends.length === 0
    ? html`<p class="muted">No backends.</p>`
    : html`<div class="scroll"><table class="routing">
<thead><tr><th>Backend</th><th class="num">In flight / cap</th><th>Deployments</th></tr></thead>
<tbody>${backends.map(
        ([id, backend]) => html`<tr>
<td class="id">${id}</td>
<td class="num">${formatCount(backend.in_flight)} / ${backend.max_in_flight === undefined ? "—" : formatCount(backend.max_in_flight)}</td>
<td>${deploymentsCell(backend.deployments, now)}</td>
</tr>`,
      )}</tbody>
</table></div>`
}
<p>Queued: ${queued.length === 0 ? html`<span class="muted">none</span>` : queued.map(([model, { queued: n }], index) => html`${index > 0 && " · "}<span class="id">${model}</span> <span class="tag warn">${formatCount(n)}</span>`)}</p>`;
}

// A backend's deployments by their model name on the backend: an open or half-open
// circuit is flagged with when it opened; a closed one is the plain name.
function deploymentsCell(deployments: Record<string, DeploymentStatus>, now: number): Markup {
  const entries = sortedEntries(deployments);
  if (entries.length === 0) return html`<span class="muted">none in the applied config</span>`;
  return html`${entries.map(([model, deployment], index) => {
    const name = html`${index > 0 && html`<br>`}<span class="id">${model}</span>`;
    if (deployment.circuit === "closed") return name;
    const openedAt = parseTimestamp(deployment.opened_at);
    const tag = deployment.circuit === "open" ? html`<span class="tag bad">circuit open</span>` : html`<span class="tag warn">circuit half-open</span>`;
    return html`${name} ${tag} since ${openedAt === undefined ? deployment.opened_at : timeAgo(openedAt, now)}`;
  })}`;
}

function sortedEntries<T>(record: Record<string, T>): [string, T][] {
  return Object.entries(record).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

function sum(values: number[]): number {
  return values.reduce((total, n) => total + n, 0);
}

// ---- Config

async function renderConfig({ core, configFile, clock }: PageSources): Promise<Markup> {
  const current = await core.currentConfig();
  const file = configFile();
  const now = clock();
  const failure = file.lastFailure;
  const heading = html`<h2>Config ${current && html`<span class="muted"><code>${shortHash(current.hash)}</code></span>`}</h2>`;
  const source = html`<p>From <code>${file.path}</code>${current && html`; published ${timeAgo(current.publishedAt, now)}`}.</p>`;
  const rejected =
    failure &&
    html`<div class="error"><strong>The latest edit was rejected (${failure.error.code})</strong> — ${failure.trigger}, ${timeAgo(failure.at, now)}.
${current ? html`Gateways keep the current config.` : html`Nothing is published: gateways wait for a config until the file is fixed.`}
<p>${failure.error.message}</p>
${failure.error.code === "config-invalid" && html`<ul>${failure.error.issues.map((issue) => html`<li><code>${issue.path || "/"}</code> ${issue.code}: ${issue.message}</li>`)}</ul>`}</div>`;
  if (!current) return html`${heading}${source}${rejected}<p class="muted">No config published yet.</p>`;
  return html`${heading}${source}${rejected}${configSummary(current.config)}`;
}

function configSummary(config: Config): Markup {
  const backends = Object.entries(config.backends);
  const models = Object.entries(config.models);
  const groups = Object.keys(config.groups ?? {});
  const keys = Object.entries(config.keys);
  return html`<p class="counts">${counted(backends.length, "backend")} · ${counted(models.length, "model")} · ${counted(keys.length, "key")} · ${counted(groups.length, "group")} <span class="muted">— limits under Totals vs limits</span></p>
<div class="grid">
<div><h3>Models</h3><ul>${models.map(([name, model]) => html`<li><span class="id">${name}</span> → ${model.deployments.map((deployment, index) => html`${index > 0 && ", "}${deployment.backend}/${deployment.model}`)}</li>`)}</ul></div>
<div><h3>Backends</h3><ul>${backends.map(([name, backend]) => html`<li><span class="id">${name}</span> ${backend.type} <span class="muted">${formatUrl(backend.base_url)}</span></li>`)}</ul></div>
<div><h3>Keys</h3>${keys.length === 0 ? html`<p class="muted">None.</p>` : html`<ul>${keys.map(([id, key]) => html`<li><span class="id">${id}</span> → group ${key.group}${key.disabled === true && html` <span class="tag bad">disabled</span>`}${key.expires_at !== undefined && html` <span class="muted">expires ${key.expires_at}</span>`}</li>`)}</ul>`}</div>
</div>
<h3>Groups</h3>${groupTree(config)}`;
}

// The groups as nested lists, top-level groups first and each group's children under
// it, siblings in config order. The nesting comes from the resolver's paths, so the
// page draws the tree the limits and model lists are resolved on. Labels are shown as
// written; the page gives none of them a meaning.
function groupTree(config: Config): Markup {
  const scopes = resolveScopes(config).filter((scope) => scope.group !== undefined);
  if (scopes.length === 0) return html`<p class="muted">None.</p>`;
  const keysByGroup = new Map<string, string[]>();
  for (const [id, key] of Object.entries(config.keys)) keysByGroup.set(key.group, [...(keysByGroup.get(key.group) ?? []), id]);
  const childrenOf = (path: readonly string[]): ResolvedScope[] =>
    scopes.filter((scope) => scope.path.length === path.length + 1 && path.every((id, index) => scope.path[index] === id));
  const branch = (path: readonly string[]): Markup => {
    const children = childrenOf(path);
    if (children.length === 0) return html``;
    return html`<ul class="tree">${children.map((scope) => html`<li>${groupLine(scope, config, keysByGroup.get(scope.group ?? "") ?? [])}${branch(scope.path)}</li>`)}</ul>`;
  };
  return branch([]);
}

function groupLine(scope: ResolvedScope, config: Config, keys: string[]): Markup {
  const labels = Object.entries(config.groups?.[scope.group ?? ""]?.labels ?? {});
  const models =
    scope.allowed_models === undefined
      ? html`<span class="muted">all models</span>`
      : scope.allowed_models.length === 0
        ? html`<span class="tag bad">no models</span>`
        : scope.allowed_models.join(", ");
  return html`<span class="id">${scope.group}</span>${labels.map(([key, value]) => html` <span class="tag label">${key}=${value}</span>`)}
<span class="muted">models:</span> ${models} <span class="muted">keys:</span> ${keys.length === 0 ? html`<span class="muted">none</span>` : keys.map((id, index) => html`${index > 0 && ", "}<span class="id">${id}</span>`)}`;
}

// A group path as the page writes it: the ancestors, then the group itself.
function groupPath(path: readonly string[]): Markup {
  return html`${path.slice(0, -1).map((id) => html`<span class="muted">${id} /</span> `)}<span class="id">${path.at(-1)}</span>`;
}

function counted(n: number, noun: string): string {
  return `${n} ${noun}${n === 1 ? "" : "s"}`;
}

// ---- Totals vs limits

// The totals carry counted_through for the gateway reading them; the page reads them
// under no gateway's name and ignores it.
const PAGE_READER = "";

const HOUR_MS = 3_600_000;

async function renderTotals({ core, clock }: PageSources): Promise<Markup> {
  const heading = html`<h2>Totals vs limits</h2>`;
  const [current, totals] = await Promise.all([core.currentConfig(), core.totals(PAGE_READER)]);
  if (!current) return html`${heading}<p class="muted">No config published yet.</p>`;
  const now = clock();
  const used = usedByLimit(totals);
  const rows: Markup[] = [];
  for (const { group, path, limits } of resolveScopes(current.config)) {
    const label = group === undefined ? html`global` : groupPath(path);
    for (const limit of limits) rows.push(limitRow(label, limit, used.get(limitKey(group, limit)), now));
  }
  const intro = html`<p class="muted">Current windows: tokens per UTC hour, dollars per UTC month, counted from every gateway's usage (${formatCount(totals.live_gateways)} live). Per-minute limits are enforced by each gateway on its share and not counted here.</p>`;
  if (rows.length === 0) return html`${heading}${intro}<p class="muted">The config sets no limits.</p>`;
  return html`${heading}${intro}
<div class="scroll"><table>
<thead><tr><th>Scope</th><th>Limit</th><th>Used</th><th class="bar">Of limit</th><th>Window from</th></tr></thead>
<tbody>${rows}</tbody>
</table></div>`;
}

interface UsedWindow {
  used: bigint;
  windowStart: string;
}

// A limit's identity: its group (none for global) and type.
function limitKey(group: string | undefined, limit: Pick<Limit, "type">): string {
  return JSON.stringify([group ?? null, limit.type]);
}

// The totals list only windows with usage; limits missing here have used nothing yet.
function usedByLimit(totals: Totals): Map<string, UsedWindow> {
  const used = new Map<string, UsedWindow>();
  for (const window of totals.windows) {
    used.set(limitKey(window.group, window), { used: BigInt(window.used), windowStart: window.window_start });
  }
  return used;
}

function limitRow(label: Markup, limit: Limit, window: UsedWindow | undefined, now: number): Markup {
  if (limit.type !== "tokens_per_hour" && limit.type !== "usd_per_month") {
    const unit = limit.type === "requests_per_minute" ? "requests" : "tokens";
    return html`<tr><td>${label}</td><td>${formatCount(limit.value)} ${unit} / min</td><td colspan="3" class="muted">per gateway share, not counted here</td></tr>`;
  }
  const used = window?.used ?? 0n;
  const money = limit.type === "usd_per_month";
  const ceiling = money ? usdToNano(limit.value) : BigInt(limit.value);
  const format = (amount: bigint): string => (money ? formatNanoUsd(amount) : formatCount(amount));
  const windowStart = window ? parseTimestamp(window.windowStart) : undefined;
  const start = windowStart ?? (money ? monthStart(now) : Math.floor(now / HOUR_MS) * HOUR_MS);
  return html`<tr><td>${label}</td><td>${format(ceiling)} ${money ? "/ month" : "tokens / hour"}</td>
<td class="num">${format(used)}</td><td class="bar">${share(used, ceiling)}</td><td>${formatAbsolute(start)}</td></tr>`;
}

function share(used: bigint, ceiling: bigint): Markup {
  if (ceiling === 0n) return html`<span class="tag bad">limit 0: blocked</span>`;
  const hundredths = Number((used * 10_000n) / ceiling);
  const percent = hundredths / 100;
  return html`<meter min="0" max="100" low="75" high="90" optimum="0" value="${Math.min(percent, 100)}"></meter> <span class="num">${percent.toFixed(percent >= 10 ? 0 : 1)}%</span>`;
}

function monthStart(now: number): number {
  const date = new Date(now);
  return Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1);
}

// ---- Recent usage

async function renderUsage({ core, clock }: PageSources): Promise<Markup> {
  const records = await core.recentRecords();
  const now = clock();
  const heading = html`<h2>Recent usage <span class="muted">last ${records.length}</span></h2>`;
  if (records.length === 0) return html`${heading}<p class="muted">No usage received yet.</p>`;
  return html`${heading}
<div class="scroll"><table>
<thead><tr><th>Received</th><th>Gateway</th><th>Key ID</th><th>Group</th><th>Model</th><th>Deployment</th><th class="num">In</th><th class="num">Cached</th><th class="num">Cache write</th><th class="num">Out</th><th class="num">Reasoning</th><th class="num">Cost</th><th>Flags</th></tr></thead>
<tbody>${records.map((received) => usageRow(received, now))}</tbody>
</table></div>`;
}

function usageRow({ receivedAt, record }: ReceivedRecord, now: number): Markup {
  const { units } = record;
  const flags = [record.estimated && "estimated", record.partial && "partial"].filter((flag) => flag !== false);
  return html`<tr>
<td>${timeAgo(receivedAt, now)}</td>
<td class="id">${record.gateway_instance}</td>
<td class="id">${record.key_id}</td>
<td>${groupPath(record.groups)}</td>
<td class="id">${record.model}</td>
<td>${record.deployment.backend}/${record.deployment.model}</td>
<td class="num">${formatCount(units.tokens_in)}</td>
<td class="num">${formatCount(units.tokens_cached)}</td>
<td class="num">${formatCount(units.tokens_cache_write)}</td>
<td class="num">${formatCount(units.tokens_out)}</td>
<td class="num">${formatCount(units.tokens_reasoning)}</td>
<td class="num">${formatNanoUsd(BigInt(record.cost_nano_usd))}</td>
<td>${flags.map((flag) => html`<span class="tag warn">${flag}</span> `)}</td>
</tr>`;
}
