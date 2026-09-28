// How the page writes amounts and times.

import { html } from "./html.ts";
import type { Markup } from "./html.ts";

const NANO_PER_USD = 1_000_000_000n;
const INTEGER = new Intl.NumberFormat("en-US");

// Tokens and other counts, with thousands separators.
export function formatCount(n: number | bigint): string {
  return INTEGER.format(n);
}

// Nano-USD as dollars, exact at any size: at least two decimals, up to nine, trailing
// zeros dropped ("$0.001524", "$12.50", "$1,000,000.00").
export function formatNanoUsd(nano: bigint): string {
  const sign = nano < 0n ? "-" : "";
  const abs = nano < 0n ? -nano : nano;
  const fraction = (abs % NANO_PER_USD).toString().padStart(9, "0").replace(/0+$/, "").padEnd(2, "0");
  return `${sign}$${INTEGER.format(abs / NANO_PER_USD)}.${fraction}`;
}

// A usd_per_month limit value (dollars, possibly fractional) in nano-USD.
export function usdToNano(usd: number): bigint {
  return BigInt(Math.round(usd * 1e9));
}

// "2026-09-24 10:00:01 UTC" — seconds are enough to read.
export function formatAbsolute(at: number): string {
  return `${new Date(at).toISOString().slice(0, 19).replace("T", " ")} UTC`;
}

// How long ago `at` was, from `now`. The page's script has the same rule
// (document.ts) and refreshes these every few seconds; the two must agree.
export function formatRelative(at: number, now: number): string {
  const seconds = Math.round((now - at) / 1000);
  if (seconds < 1) return "just now";
  if (seconds < 60) return `${seconds} s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
  return `${Math.floor(seconds / 86400)} d ago`;
}

// A moment as relative time (refreshed by the page's script) with the absolute time
// beside it.
export function timeAgo(at: number, now: number): Markup {
  const iso = new Date(at).toISOString();
  return html`<time datetime="${iso}" data-relative>${formatRelative(at, now)}</time> <span class="muted">${formatAbsolute(at)}</span>`;
}

// A protocol timestamp (RFC 3339, possibly with nanoseconds) as a moment; undefined
// when the platform cannot read it.
export function parseTimestamp(text: string): number | undefined {
  const at = Date.parse(text);
  return Number.isNaN(at) ? undefined : at;
}

// A backend URL as the page shows it: without userinfo, whatever the config holds —
// credentials in a URL are rejected by validation, and the page never prints them.
export function formatUrl(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return "(not a URL)";
  }
  url.username = "";
  url.password = "";
  return url.href;
}
