// The page around the sections: system fonts, a small inline stylesheet (light and
// dark follow the browser), and a small inline script that applies section updates
// from /events and keeps relative times current. No build step, nothing loaded from
// elsewhere; the Content-Security-Policy allows exactly these two inline blocks.

import { createHash } from "node:crypto";

import { html, markupText } from "./html.ts";
import type { Markup } from "./html.ts";
import { SECTION_IDS } from "./sections.ts";
import type { SectionId } from "./sections.ts";

const STYLE = `
:root { color-scheme: light dark; --fg: #1d1f23; --muted: #666b73; --bg: #fafafa; --card: #fff; --line: #e2e4e8;
  --ok: #1f7a3f; --warn: #9a6200; --bad: #b3261e; --tint: rgba(0,0,0,.04); }
@media (prefers-color-scheme: dark) {
  :root { --fg: #e6e7ea; --muted: #9aa0a8; --bg: #16181b; --card: #1e2125; --line: #33373d;
    --ok: #5cc285; --warn: #e0a84a; --bad: #f0786f; --tint: rgba(255,255,255,.05); }
}
* { box-sizing: border-box; }
body { margin: 0; padding: 16px; background: var(--bg); color: var(--fg);
  font: 14px/1.45 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
header, main, footer { max-width: 1200px; margin: 0 auto; }
header { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 12px; margin-bottom: 12px; }
h1 { font-size: 20px; margin: 0; }
h2 { font-size: 16px; margin: 0 0 8px; }
h3 { font-size: 13px; margin: 8px 0 4px; text-transform: uppercase; letter-spacing: .04em; color: var(--muted); }
section { background: var(--card); border: 1px solid var(--line); border-radius: 8px; padding: 12px 14px; margin-bottom: 14px; }
p { margin: 4px 0 8px; }
ul { margin: 0; padding-left: 18px; }
code, .id, .num { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 13px; }
.num { text-align: right; white-space: nowrap; }
.muted { color: var(--muted); }
.scroll { overflow-x: auto; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 5px 8px; border-bottom: 1px solid var(--line); vertical-align: top; }
th { font-weight: 600; white-space: nowrap; }
tbody tr:hover { background: var(--tint); }
th.num { text-align: right; }
td.bar { white-space: nowrap; }
meter { width: 120px; vertical-align: middle; }
.tag { display: inline-block; padding: 0 6px; border-radius: 4px; font-size: 12px; border: 1px solid currentColor; white-space: nowrap; }
.ok, .state-ready { color: var(--ok); }
.warn, .state-starting, .state-draining { color: var(--warn); }
.bad { color: var(--bad); }
.error { border-left: 3px solid var(--bad); background: var(--tint); padding: 8px 12px; margin: 8px 0; }
.notice { max-width: 1200px; margin: 0 auto 14px; border-left: 3px solid var(--warn); background: var(--tint); padding: 8px 12px; }
.tree li { margin: 2px 0; }
.label { color: var(--muted); }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 0 18px; }
footer { color: var(--muted); font-size: 12px; }
`;

// Kept small and dependency-free. relative() follows formatRelative in format.ts.
const SCRIPT = `
(() => {
  const ids = new Set(${JSON.stringify(SECTION_IDS)});
  const connection = document.getElementById("connection");
  const relative = (at, now) => {
    const s = Math.round((now - at) / 1000);
    if (s < 1) return "just now";
    if (s < 60) return s + " s ago";
    if (s < 3600) return Math.floor(s / 60) + " min ago";
    if (s < 86400) return Math.floor(s / 3600) + " h ago";
    return Math.floor(s / 86400) + " d ago";
  };
  const refreshTimes = () => {
    const now = Date.now();
    for (const el of document.querySelectorAll("time[data-relative]")) {
      const at = Date.parse(el.getAttribute("datetime"));
      if (!Number.isNaN(at)) el.textContent = relative(at, now);
    }
  };
  setInterval(refreshTimes, 5000);
  const events = new EventSource("events");
  events.addEventListener("open", () => { connection.textContent = "live"; connection.className = "tag ok"; });
  events.addEventListener("error", () => { connection.textContent = "reconnecting"; connection.className = "tag warn"; });
  events.addEventListener("section", (event) => {
    const { id, html } = JSON.parse(event.data);
    const section = ids.has(id) && document.getElementById(id);
    if (!section) return;
    section.innerHTML = html;
    refreshTimes();
  });
})();
`;

const hashOf = (text: string): string => `'sha256-${createHash("sha256").update(text, "utf8").digest("base64")}'`;

export const CONTENT_SECURITY_POLICY = [
  "default-src 'none'",
  `style-src ${hashOf(STYLE)}`,
  `script-src ${hashOf(SCRIPT)}`,
  "connect-src 'self'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
].join("; ");

// Style and script are constants of this module; html cannot interpolate them
// unescaped, so the page is assembled around the rendered body here.
export function pageDocument(sections: ReadonlyMap<SectionId, Markup>): string {
  const body = html`<header>
<h1>kaiak sample control plane</h1>
<span id="connection" class="tag warn">connecting</span>
</header>
<p class="notice"><strong>State lives in memory.</strong> Budgets, usage totals and the de-duplication of usage batches reset when this process restarts: the sample is for demos and validation, not a billing system.</p>
<main>${SECTION_IDS.map((id) => html`<section id="${id}">${sections.get(id)}</section>`)}</main>
<footer><p>No authentication: this page is for local and demo use — anyone who can reach this address can read it. It shows key IDs, never keys.</p></footer>`;
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>kaiak sample control plane</title>
<style>${STYLE}</style>
</head>
<body>
${markupText(body)}
<script>${SCRIPT}</script>
</body>
</html>
`;
}
