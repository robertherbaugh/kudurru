// A tagged template for HTML with escaping. Interpolated strings, numbers
// and arrays of them are escaped; values built with `html` or `raw` are
// inserted as they are. The pages are rendered once, at build time.

const SAFE = Symbol("html");

export interface Html {
  readonly [SAFE]: true;
  readonly value: string;
}

type Part = Html | string | number | false | null | undefined | readonly Part[];

const ENTITIES: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

export function escape(text: string): string {
  return text.replace(/[&<>"']/g, (c) => ENTITIES[c] ?? c);
}

export function raw(value: string): Html {
  return { [SAFE]: true, value };
}

function render(part: Part): string {
  if (part === false || part === null || part === undefined) return "";
  if (Array.isArray(part)) return part.map(render).join("");
  if (typeof part === "object" && SAFE in part) return part.value;
  return escape(String(part));
}

export function html(strings: TemplateStringsArray, ...parts: Part[]): Html {
  let out = strings[0] ?? "";
  parts.forEach((part, i) => {
    out += render(part) + (strings[i + 1] ?? "");
  });
  return raw(out);
}
