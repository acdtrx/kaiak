// The control plane's limit windows (GATEWAY.md, Limits): fixed UTC hours for
// tokens_per_hour, UTC calendar months for usd_per_month, by its own clock.

import { COUNTED_TYPES } from "../messages/index.ts";
import type { TotalsLimitType } from "../messages/index.ts";
import type { WindowStarts } from "../storage/index.ts";

const HOUR_MS = 3_600_000;

// The windows `now` (milliseconds since the epoch) falls in. Epoch milliseconds carry
// no leap seconds, so every UTC hour is 3,600,000 of them.
export function currentWindows(now: number): WindowStarts {
  const date = new Date(now);
  return {
    tokens_per_hour: Math.floor(now / HOUR_MS) * HOUR_MS,
    usd_per_month: Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1),
  };
}

// The windows just before `current`: the previous hour and the previous month.
export function previousWindows(current: WindowStarts): WindowStarts {
  const month = new Date(current.usd_per_month);
  return {
    tokens_per_hour: current.tokens_per_hour - HOUR_MS,
    usd_per_month: Date.UTC(month.getUTCFullYear(), month.getUTCMonth() - 1, 1),
  };
}

export function sameWindows(a: WindowStarts, b: WindowStarts): boolean {
  return COUNTED_TYPES.every((type) => a[type] === b[type]);
}

// The window a record settled at `gatewayTime` counts in (docs/specs/CONTROL-PROTOCOL.md,
// Usage intake → Counted in its own window): its own window when that is the current
// or the previous one, otherwise the current one — so a backlog delivered after an
// outage lands in the hours and months it was used in, while usage older than the
// previous window (which nothing enforces any more) and a gateway clock ahead of the
// control plane's count now.
export function recordWindowStart(type: TotalsLimitType, gatewayTime: string, current: WindowStarts): number {
  const own = currentWindows(Date.parse(`${gatewayTime.slice(0, 19)}Z`))[type];
  const previous = previousWindows(current)[type];
  return own === previous ? previous : current[type];
}

// A window start as the protocol writes it: whole seconds, UTC ("2026-09-24T10:00:00Z").
export function formatWindowStart(start: number): string {
  return `${new Date(start).toISOString().slice(0, 19)}Z`;
}

// Each type's window start as the protocol writes it.
export function formatWindowStarts(windows: WindowStarts): Record<TotalsLimitType, string> {
  return {
    tokens_per_hour: formatWindowStart(windows.tokens_per_hour),
    usd_per_month: formatWindowStart(windows.usd_per_month),
  };
}
