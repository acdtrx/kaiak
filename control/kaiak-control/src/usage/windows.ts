// The control plane's limit windows (GATEWAY.md, Limits): fixed UTC hours for
// tokens_per_hour, UTC calendar months for usd_per_month, by its own clock.

import type { TotalsLimitType } from "../messages/index.ts";
import type { CurrentWindows } from "../storage/index.ts";

const HOUR_MS = 3_600_000;

// The windows `now` (milliseconds since the epoch) falls in. Epoch milliseconds carry
// no leap seconds, so every UTC hour is 3,600,000 of them.
export function currentWindows(now: number): CurrentWindows {
  const date = new Date(now);
  return {
    hourStart: Math.floor(now / HOUR_MS) * HOUR_MS,
    monthStart: Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1),
  };
}

export function windowStartFor(type: TotalsLimitType, windows: CurrentWindows): number {
  return type === "tokens_per_hour" ? windows.hourStart : windows.monthStart;
}

// The windows just before `current`: the previous hour and the previous month.
export function previousWindows(current: CurrentWindows): CurrentWindows {
  const month = new Date(current.monthStart);
  return {
    hourStart: current.hourStart - HOUR_MS,
    monthStart: Date.UTC(month.getUTCFullYear(), month.getUTCMonth() - 1, 1),
  };
}

// The window a record settled at `gatewayTime` counts in (docs/specs/CONTROL-PROTOCOL.md,
// Usage intake → Counted in its own window): its own window when that is the current
// or the previous one, otherwise the current one — so a backlog delivered after an
// outage lands in the hours and months it was used in, while usage older than the
// previous window (which nothing enforces any more) and a gateway clock ahead of the
// control plane's count now.
export function recordWindowStart(type: TotalsLimitType, gatewayTime: string, current: CurrentWindows): number {
  const own = windowStartFor(type, currentWindows(Date.parse(`${gatewayTime.slice(0, 19)}Z`)));
  const previous = windowStartFor(type, previousWindows(current));
  return own === previous ? previous : windowStartFor(type, current);
}

// A window start as the protocol writes it: whole seconds, UTC ("2026-09-24T10:00:00Z").
export function formatWindowStart(start: number): string {
  return `${new Date(start).toISOString().slice(0, 19)}Z`;
}
