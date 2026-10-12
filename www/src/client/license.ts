// The "Request a key" form of /license/. A personal request is sent to the
// license server (CORS, JSON); a business or other request is not: it opens
// the visitor's mail program with every field filled in, addressed to the
// licensing contact, so nothing about a company goes to a service the
// visitor did not see. The functions at the top are pure and tested; the
// page wiring (initLicenseForm) is at the bottom.

export const CONTACT = "hello@kudurru.io";
export const BUSINESS_SUBJECT = "Business License";
export const PERSONAL_SUBJECT = "Free personal key";

export type Kind = "personal" | "business";

export interface Fields {
  kind: Kind;
  name: string;
  email: string;
  // Personal: what it runs for (required). Business: the details (features, timeline).
  use: string;
  company: string;
  // Business only: a personal key always covers the personal maximum.
  nodes: string;
  noncommercial: boolean;
  // The honeypot: a person leaves it empty.
  website: string;
}

// The personal limits are the license server's (POST /api/v1/public/key-requests).
export const LIMITS = { name: 200, email: 254, company: 160, personalUse: 1000, businessDetails: 1500, maxNodes: 10000 };
export const PERSONAL_NODES = 3;
// The path of the server's public form endpoint, below its origin.
export const REQUEST_PATH = "/api/v1/public/key-requests";

const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** What is wrong with the form, one sentence each; empty when it can be sent. */
export function validate(f: Fields): string[] {
  const out: string[] = [];
  const name = f.name.trim();
  if (!name) out.push("Enter your name.");
  else if (name.length > LIMITS.name) out.push(`The name is longer than ${LIMITS.name} characters.`);
  const email = f.email.trim();
  if (!email) out.push("Enter your email address.");
  else if (!EMAIL.test(email) || email.length > LIMITS.email) out.push("Enter a valid email address.");
  const use = f.use.trim();
  if (f.kind === "personal") {
    if (!use) out.push("Say what you run kudurru for.");
    else if (use.length > LIMITS.personalUse) out.push(`What you run it for is longer than ${LIMITS.personalUse} characters.`);
    if (!f.noncommercial) out.push("Agree and certify that the key is only for personal, non-commercial use.");
  } else {
    if (!f.company.trim()) out.push("Enter the name of your business or organization.");
    else if (f.company.trim().length > LIMITS.company) out.push(`The business name is longer than ${LIMITS.company} characters.`);
    const nodes = Number(f.nodes);
    if (!f.nodes.trim() || !Number.isInteger(nodes) || nodes < 1 || nodes > LIMITS.maxNodes) out.push("Enter the number of nodes, a whole number of 1 or more.");
    if (use.length > LIMITS.businessDetails) out.push(`The details are longer than ${LIMITS.businessDetails} characters.`);
  }
  return out;
}

/** The JSON a personal request sends to the license server (its contract: name, email, use, website, and the ticked certification). */
export function personalPayload(f: Fields): Record<string, unknown> {
  // noncommercial is the certification the visitor ticked (the server may keep it).
  return { name: f.name.trim(), email: f.email.trim(), use: f.use.trim(), noncommercial: f.noncommercial, website: f.website };
}

/** The URL of the server's form endpoint for a license server origin. */
export function requestURL(origin: string): string {
  return new URL(REQUEST_PATH, origin).toString();
}

/** The text of a business request: the same in the mail body and in the copy box. */
export function businessBody(f: Fields): string {
  const lines = [
    `Name: ${f.name.trim()}`,
    `Business: ${f.company.trim()}`,
    `Email: ${f.email.trim()}`,
    `Nodes: ${Number(f.nodes)}`,
  ];
  const details = f.use.trim();
  if (details) lines.push("", "Details:", details);
  return lines.join("\n");
}

/** mailto: with the subject and every field in the body (RFC 6068). */
export function mailto(subject: string, body: string): string {
  return `mailto:${CONTACT}?subject=${encodeURIComponent(subject)}&body=${encodeURIComponent(body)}`;
}

export interface Outcome {
  ok: boolean;
  message: string;
  // True when the visitor should write an email instead (server unreachable).
  fallback?: boolean;
}

const FIELD_LABEL: Record<string, string> = { name: "your name", email: "your email address", use: "what you run it for", noncommercial: "the non-commercial certification" };

/** Sends a personal request; never throws. */
export async function sendPersonal(endpoint: string, f: Fields, fetchImpl: typeof fetch = fetch): Promise<Outcome> {
  let res: Response;
  try {
    res = await fetchImpl(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(personalPayload(f)),
      credentials: "omit",
      referrerPolicy: "no-referrer",
    });
  } catch {
    return { ok: false, fallback: true, message: "The license server could not be reached." };
  }
  if (res.status === 202) {
    return { ok: true, message: "Check your inbox. We sent a link to confirm your address; your key arrives by email after you confirm. Look in your spam folder if nothing arrives in a few minutes." };
  }
  if (res.status === 429) {
    const wait = Number(res.headers.get("Retry-After"));
    const when = Number.isFinite(wait) && wait > 0 ? ` Try again in ${wait < 90 ? `${Math.ceil(wait)} seconds` : `${Math.ceil(wait / 60)} minutes`}.` : " Try again in a few minutes.";
    return { ok: false, message: `Too many requests from your network.${when}` };
  }
  let body: { error?: { message?: unknown; fields?: unknown } } = {};
  try {
    body = await res.json();
  } catch {
    // not JSON
  }
  if (res.status === 400) {
    const fields = Array.isArray(body.error?.fields) ? (body.error!.fields as unknown[]).filter((x): x is string => typeof x === "string") : [];
    const named = fields.map((x) => FIELD_LABEL[x]).filter(Boolean);
    const detail = typeof body.error?.message === "string" ? body.error.message : "";
    return { ok: false, message: named.length ? `The license server did not accept: ${named.join(", ")}. ${detail}`.trim() : detail || "The license server did not accept the request. Check the form and try again." };
  }
  // 503 (no signing key or no mail configured), any other status: write instead.
  return { ok: false, fallback: true, message: "The license server cannot take requests right now." };
}

// --- the page --------------------------------------------------------------

function read(form: HTMLFormElement): Fields {
  const get = (n: string) => (form.elements.namedItem(n) as HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement | null)?.value ?? "";
  const kind = (form.querySelector<HTMLInputElement>('input[name="kind"]:checked')?.value ?? "personal") as Kind;
  const panel = (k: Kind, n: string) => get(`${k}-${n}`);
  return {
    kind,
    name: get("name"),
    email: get("email"),
    use: panel(kind, "use"),
    company: get("company"),
    nodes: get("business-nodes"),
    noncommercial: (form.elements.namedItem("noncommercial") as HTMLInputElement | null)?.checked ?? false,
    website: get("website"),
  };
}

export function initLicenseForm(root: ParentNode = document): void {
  const form = root.querySelector<HTMLFormElement>("[data-license-form]");
  if (!form) return;
  const endpoint = form.dataset.endpoint ?? ""; // the server's full form URL, from the page
  const status = form.querySelector<HTMLElement>("[data-status]")!;
  const problems = form.querySelector<HTMLElement>("[data-problems]")!;
  const mailBox = form.querySelector<HTMLElement>("[data-mail]")!;
  const mailText = form.querySelector<HTMLElement>("[data-mail-text]")!;
  const submit = form.querySelector<HTMLButtonElement>('button[type="submit"]')!;
  const panels = new Map<Kind, HTMLElement>([
    ["personal", form.querySelector<HTMLElement>('[data-panel="personal"]')!],
    ["business", form.querySelector<HTMLElement>('[data-panel="business"]')!],
  ]);

  // Only the chosen panel's fields exist for the browser: hidden ones are disabled.
  const showKind = () => {
    const kind = read(form).kind;
    for (const [k, el] of panels) {
      el.hidden = k !== kind;
      for (const input of el.querySelectorAll<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>("input, textarea, select")) input.disabled = k !== kind;
    }
    submit.textContent = kind === "personal" ? "Request a key" : "Write to us";
    status.textContent = "";
    problems.replaceChildren();
    mailBox.hidden = true;
  };
  for (const r of form.querySelectorAll<HTMLInputElement>('input[name="kind"]')) r.addEventListener("change", showKind);
  showKind();

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = read(form);
    problems.replaceChildren();
    status.textContent = "";
    mailBox.hidden = true;
    // A bot filled the honeypot: look like success, send nothing.
    if (f.website) {
      status.textContent = "Thank you.";
      return;
    }
    const issues = validate(f);
    if (issues.length) {
      for (const text of issues) {
        const li = document.createElement("li");
        li.textContent = text;
        problems.append(li);
      }
      problems.closest("[data-problems-box]")?.removeAttribute("hidden");
      status.textContent = issues.length === 1 ? "There is 1 problem with the form." : `There are ${issues.length} problems with the form.`;
      return;
    }
    problems.closest("[data-problems-box]")?.setAttribute("hidden", "");
    if (f.kind === "business") {
      const body = businessBody(f);
      mailText.textContent = body;
      mailBox.hidden = false;
      status.textContent = `Your mail program should open with the details filled in. If it does not, send the text below to ${CONTACT} with the subject ${BUSINESS_SUBJECT}.`;
      window.location.href = mailto(BUSINESS_SUBJECT, body);
      return;
    }
    submit.disabled = true;
    status.textContent = "Sending…";
    const out = await sendPersonal(endpoint, f);
    submit.disabled = false;
    status.textContent = out.message;
    if (out.ok) {
      form.reset();
      showKind();
      status.textContent = out.message;
    } else if (out.fallback) {
      const body = [`Name: ${f.name.trim()}`, `Email: ${f.email.trim()}`, "", "Use:", f.use.trim()].join("\n");
      mailText.textContent = body;
      mailBox.hidden = false;
      status.textContent = `${out.message} Send the text below to ${CONTACT} with the subject ${PERSONAL_SUBJECT} instead, or try again later.`;
    }
  });
}
