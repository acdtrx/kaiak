// The control plane's limit windows (GATEWAY.md, Limits): fixed UTC hours for the
// per-hour types (tokens, images, audio generated, audio processed), UTC calendar
// months for usd_per_month, by its own clock.

import { COUNTED_TYPES } from "../messages/index.ts";
import type { TotalsLimitType } from "../messages/index.ts";
import type { WindowStarts } from "../storage/index.ts";

const HOUR_MS = 3_600_000;

// The windows `now` (milliseconds since the epoch) falls in. Epoch milliseconds carry
// no leap seconds, so every UTC hour is 3,600,000 of them.
export function currentWindows(now: number): WindowStarts {
  const date = new Date(now);
  return windowsAt(Math.floor(now / HOUR_MS) * HOUR_MS, Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1));
}

// The windows just before `current`: the previous hour and the previous month.
export function previousWindows(current: WindowStarts): WindowStarts {
  const month = new Date(current.usd_per_month);
  return windowsAt(current.tokens_per_hour - HOUR_MS, Date.UTC(month.getUTCFullYear(), month.getUTCMonth() - 1, 1));
}

// Every per-hour type's window starting at `hour`, and usd_per_month's at `month`.
function windowsAt(hour: number, month: number): WindowStarts {
  return {
    tokens_per_hour: hour,
    images_per_hour: hour,
    audio_seconds_out_per_hour: hour,
    audio_seconds_in_per_hour: hour,
    usd_per_month: month,
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
    images_per_hour: formatWindowStart(windows.images_per_hour),
    audio_seconds_out_per_hour: formatWindowStart(windows.audio_seconds_out_per_hour),
    audio_seconds_in_per_hour: formatWindowStart(windows.audio_seconds_in_per_hour),
    usd_per_month: formatWindowStart(windows.usd_per_month),
  };
}
