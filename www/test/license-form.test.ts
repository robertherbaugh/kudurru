import { describe, expect, test } from "bun:test";
import {
  BUSINESS_SUBJECT,
  businessBody,
  mailto,
  personalPayload,
  requestURL,
  sendPersonal,
  validate,
  type Fields,
} from "../src/client/license.ts";

const personal: Fields = {
  kind: "personal", name: "Homelab Hank", email: "hank@example.org", use: "DNS for my home network",
  company: "", nodes: "", noncommercial: true, website: "",
};
const business: Fields = {
  kind: "business", name: "Ada Admin", email: "ada@example.com", use: "OIDC and the audit trail, by January",
  company: "Example GmbH", nodes: "12", noncommercial: false, website: "",
};

const reply = (status: number, body?: unknown, headers: Record<string, string> = {}) =>
  (async () => new Response(body === undefined ? null : JSON.stringify(body), { status, headers })) as unknown as typeof fetch;

describe("validate", () => {
  test("accepts a complete personal request and a complete business request", () => {
    expect(validate(personal)).toEqual([]);
    expect(validate(business)).toEqual([]);
  });

  test("a personal request needs a name, an address, a use and the non-commercial confirmation", () => {
    expect(validate({ ...personal, name: " ", email: "nope", use: "", noncommercial: false })).toEqual([
      "Enter your name.",
      "Enter a valid email address.",
      "Say what you run kudurru for.",
      "Agree and certify that the key is only for eligible community use.",
    ]);
  });

  test("a personal request has no node count: a personal key always covers the personal maximum", () => {
    expect(validate({ ...personal, nodes: "" })).toEqual([]);
    expect(validate({ ...personal, nodes: "50" })).toEqual([]);
  });

  test("a business request needs the business and a whole number of nodes", () => {
    expect(validate({ ...business, company: "" })).toContain("Enter the name of your business or organization.");
    for (const nodes of ["", "0", "-2", "1.5", "many", "10001"]) {
      expect(validate({ ...business, nodes })).toContain("Enter the number of nodes, a whole number of 1 or more.");
    }
    expect(validate({ ...business, use: "", noncommercial: false })).toEqual([]);
  });

  test("limits match the license server's: name 200, address 254, use 1000", () => {
    expect(validate({ ...personal, name: "x".repeat(201) })).toHaveLength(1);
    expect(validate({ ...personal, name: "x".repeat(200) })).toEqual([]);
    expect(validate({ ...personal, email: `${"a".repeat(250)}@b.co` })).toHaveLength(1);
    expect(validate({ ...personal, use: "x".repeat(1001) })).toHaveLength(1);
    expect(validate({ ...personal, use: "x".repeat(1000) })).toEqual([]);
  });
});

describe("the personal request", () => {
  test("sends the server's fields and the ticked certification", () => {
    expect(personalPayload({ ...personal, name: "  Homelab Hank ", use: " DNS " })).toEqual({
      name: "Homelab Hank", email: "hank@example.org", use: "DNS", noncommercial: true, website: "",
    });
  });

  test("posts to the server's path below its origin, whatever path the origin was given with", () => {
    expect(requestURL("https://licenses.kudurru.io")).toBe("https://licenses.kudurru.io/api/v1/public/key-requests");
    expect(requestURL("https://licenses.kudurru.io/")).toBe("https://licenses.kudurru.io/api/v1/public/key-requests");
  });

  test("a 202 says to check the inbox", async () => {
    let sent: { url: string; init: RequestInit } | undefined;
    const f = (async (url: string, init: RequestInit) => {
      sent = { url, init };
      return new Response(JSON.stringify({ status: "sent" }), { status: 202 });
    }) as unknown as typeof fetch;
    const out = await sendPersonal("https://licenses.kudurru.io/api/v1/public/key-requests", personal, f);
    expect(out.ok).toBe(true);
    expect(out.message).toContain("Check your inbox");
    // Personal keys are approved by an administrator: the text says so, and does not promise a key on confirmation.
    expect(out.message).toContain("we review the request and email your key once it is approved");
    expect(out.message).not.toContain("arrives by email after you confirm");
    expect(sent?.init.method).toBe("POST");
    expect((sent?.init.headers as Record<string, string>)["Content-Type"]).toBe("application/json");
    expect(sent?.init.credentials).toBe("omit");
    expect(JSON.parse(String(sent?.init.body))).toEqual({ name: "Homelab Hank", email: "hank@example.org", use: "DNS for my home network", noncommercial: true, website: "" });
  });

  test("a 400 names the fields the server refused", async () => {
    const out = await sendPersonal("u", personal, reply(400, { error: { key: "invalid", message: "check them", fields: ["email", "use", "bogus"] } }));
    expect(out).toMatchObject({ ok: false });
    expect(out.fallback).toBeUndefined();
    expect(out.message).toContain("your email address, what you run it for");
    expect(out.message).toContain("check them");
  });

  test("a 400 about the certification names the checkbox", async () => {
    const out = await sendPersonal("u", personal, reply(400, { error: { key: "invalid", message: "certify", fields: ["noncommercial"] } }));
    expect(out.message).toContain("the non-commercial certification");
  });

  test("a 429 says when to try again", async () => {
    expect((await sendPersonal("u", personal, reply(429, { error: { key: "rate_limited" } }, { "Retry-After": "45" }))).message).toContain("45 seconds");
    expect((await sendPersonal("u", personal, reply(429, undefined, { "Retry-After": "600" }))).message).toContain("10 minutes");
    expect((await sendPersonal("u", personal, reply(429))).message).toContain("a few minutes");
  });

  test("a server that is down, unconfigured or unreachable sends the visitor to email", async () => {
    for (const f of [reply(503, { error: { key: "unavailable", message: "no mail" } }), reply(500), (async () => { throw new TypeError("Failed to fetch"); }) as unknown as typeof fetch]) {
      const out = await sendPersonal("u", personal, f);
      expect(out).toMatchObject({ ok: false, fallback: true });
    }
  });
});

describe("the business request", () => {
  test("puts every field in the mail body, nothing else", () => {
    expect(businessBody(business)).toBe(
      "Name: Ada Admin\nBusiness: Example GmbH\nEmail: ada@example.com\nNodes: 12\n\nDetails:\nOIDC and the audit trail, by January",
    );
    expect(businessBody({ ...business, use: "" })).toBe("Name: Ada Admin\nBusiness: Example GmbH\nEmail: ada@example.com\nNodes: 12");
  });

  test("opens hello@kudurru.io with the subject Business License", () => {
    const url = mailto(BUSINESS_SUBJECT, businessBody(business));
    expect(url.startsWith("mailto:hello@kudurru.io?subject=Business%20License&body=")).toBe(true);
    const params = new URL(url).searchParams;
    expect(params.get("subject")).toBe("Business License");
    expect(params.get("body")).toBe(businessBody(business));
  });

  test("encodes what a person types: ampersands, plus signs, newlines and non-ASCII text", () => {
    const body = businessBody({ ...business, company: "Müller & Söhne + Co", use: "line one\nline two & three" });
    expect(new URL(mailto("Business License", body)).searchParams.get("body")).toBe(body);
    const url = mailto("Business License", body);
    expect(url).not.toMatch(/[ \n]/);
    expect(url.split("&")).toHaveLength(2); // only the one that separates subject from body
  });
});
