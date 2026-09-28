// HTML built from template literals, escaped by default: every value interpolated
// into an `html` template is escaped unless it is itself markup made by `html`. There
// is no way to mark an arbitrary string as safe, so nothing reported by a gateway or
// written in the config file can reach the page unescaped.

const MARKUP = Symbol("markup");

export interface Markup {
  readonly [MARKUP]: string;
}

// What a template may interpolate: text (escaped), markup, lists of either, and
// nothing (null, undefined and false render empty — for conditional parts).
export type Interpolation = string | number | bigint | Markup | null | undefined | false | readonly Interpolation[];

const ESCAPES: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

export function escapeHtml(text: string): string {
  return text.replace(/[&<>"']/g, (char) => ESCAPES[char] ?? char);
}

export function html(strings: TemplateStringsArray, ...values: Interpolation[]): Markup {
  let out = strings[0] ?? "";
  values.forEach((value, index) => {
    out += render(value) + (strings[index + 1] ?? "");
  });
  return { [MARKUP]: out };
}

// The markup as a string, to send.
export function markupText(markup: Markup): string {
  return markup[MARKUP];
}

function render(value: Interpolation): string {
  if (value === null || value === undefined || value === false) return "";
  if (Array.isArray(value)) return value.map(render).join("");
  if (typeof value === "object") return (value as Markup)[MARKUP];
  return escapeHtml(String(value));
}
