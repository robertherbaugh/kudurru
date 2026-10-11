// Typed per-RR editing (§10 principle 11). Every record type is a list of
// fields; compose() turns field values into the presentation-format target
// an Endpoint carries (the form internal/zone.ParseTarget accepts), parse()
// goes back, and validate() reports per-field errors before anything is
// sent. The server stays the authority: its 400 "invalid" error is shown
// verbatim when client-side checks pass but the rdata does not.

export type FieldKind = 'ipv4' | 'ipv6' | 'host' | 'uint8' | 'uint16' | 'uint32' | 'text' | 'hex' | 'enum' | 'svcparams' | 'quoted' | 'rdata';

export interface FieldSpec {
  key: string;
  label: string;
  kind: FieldKind;
  options?: string[];
  placeholder?: string;
  help?: string;
  optional?: boolean;
  /** Inclusive bounds for numeric fields. */
  min?: number;
  max?: number;
}

export interface TypeSpec {
  type: string;
  label: string;
  fields: FieldSpec[];
  /** Whether more than one target per name makes sense (CNAME: no). */
  single?: boolean;
}

export const RECORD_TYPES: TypeSpec[] = [
  { type: 'A', label: 'A (IPv4 address)', fields: [{ key: 'address', label: 'IPv4 address', kind: 'ipv4', placeholder: '192.0.2.10' }] },
  { type: 'AAAA', label: 'AAAA (IPv6 address)', fields: [{ key: 'address', label: 'IPv6 address', kind: 'ipv6', placeholder: '2001:db8::10' }] },
  { type: 'CNAME', label: 'CNAME (alias)', single: true, fields: [{ key: 'target', label: 'Canonical name', kind: 'host', placeholder: 'www.example.com.' }] },
  {
    type: 'MX',
    label: 'MX (mail exchanger)',
    fields: [
      { key: 'preference', label: 'Preference', kind: 'uint16', placeholder: '10' },
      { key: 'exchange', label: 'Mail server', kind: 'host', placeholder: 'mail.example.com.' },
    ],
  },
  {
    type: 'SRV',
    label: 'SRV (service)',
    fields: [
      { key: 'priority', label: 'Priority', kind: 'uint16', placeholder: '10' },
      { key: 'weight', label: 'Weight', kind: 'uint16', placeholder: '5' },
      { key: 'port', label: 'Port', kind: 'uint16', placeholder: '443' },
      { key: 'target', label: 'Target host', kind: 'host', placeholder: 'svc.example.com.' },
    ],
  },
  { type: 'TXT', label: 'TXT (text)', fields: [{ key: 'text', label: 'Text', kind: 'text', placeholder: 'v=spf1 -all', help: 'Stored unquoted; strings over 255 bytes are split by the server.' }] },
  {
    type: 'CAA',
    label: 'CAA (certificate authority)',
    fields: [
      { key: 'flags', label: 'Flags', kind: 'uint8', placeholder: '0' },
      { key: 'tag', label: 'Tag', kind: 'enum', options: ['issue', 'issuewild', 'iodef', 'issuemail', 'issuevmc'] },
      { key: 'value', label: 'Value', kind: 'quoted', placeholder: 'letsencrypt.org' },
    ],
  },
  {
    type: 'HTTPS',
    label: 'HTTPS (service binding)',
    fields: [
      { key: 'priority', label: 'Priority', kind: 'uint16', placeholder: '1', help: '0 is alias mode.' },
      { key: 'target', label: 'Target', kind: 'host', placeholder: '.', help: '"." means the owner name.' },
      { key: 'params', label: 'Parameters', kind: 'svcparams', optional: true, placeholder: 'alpn=h2,h3 port=443' },
    ],
  },
  {
    type: 'SVCB',
    label: 'SVCB (service binding)',
    fields: [
      { key: 'priority', label: 'Priority', kind: 'uint16', placeholder: '1' },
      { key: 'target', label: 'Target', kind: 'host', placeholder: 'svc.example.com.' },
      { key: 'params', label: 'Parameters', kind: 'svcparams', optional: true, placeholder: 'alpn=dot port=853' },
    ],
  },
  { type: 'NS', label: 'NS (name server)', fields: [{ key: 'host', label: 'Name server', kind: 'host', placeholder: 'ns1.example.com.' }] },
  { type: 'PTR', label: 'PTR (reverse pointer)', fields: [{ key: 'target', label: 'Host name', kind: 'host', placeholder: 'host.example.com.' }] },
  {
    type: 'TLSA',
    label: 'TLSA (DANE)',
    fields: [
      { key: 'usage', label: 'Usage', kind: 'uint8', max: 3, placeholder: '3' },
      { key: 'selector', label: 'Selector', kind: 'uint8', max: 1, placeholder: '1' },
      { key: 'matching', label: 'Matching type', kind: 'uint8', max: 2, placeholder: '1' },
      { key: 'data', label: 'Certificate data (hex)', kind: 'hex' },
    ],
  },
  {
    type: 'SSHFP',
    label: 'SSHFP (SSH fingerprint)',
    fields: [
      { key: 'algorithm', label: 'Algorithm', kind: 'uint8', min: 1, max: 6, placeholder: '4', help: '1 RSA, 2 DSA, 3 ECDSA, 4 Ed25519, 6 Ed448' },
      { key: 'fptype', label: 'Fingerprint type', kind: 'uint8', min: 1, max: 2, placeholder: '2', help: '1 SHA-1, 2 SHA-256' },
      { key: 'fingerprint', label: 'Fingerprint (hex)', kind: 'hex' },
    ],
  },
  {
    type: 'NAPTR',
    label: 'NAPTR (naming authority)',
    fields: [
      { key: 'order', label: 'Order', kind: 'uint16', placeholder: '100' },
      { key: 'preference', label: 'Preference', kind: 'uint16', placeholder: '10' },
      { key: 'flags', label: 'Flags', kind: 'quoted', optional: true, placeholder: 'S' },
      { key: 'service', label: 'Service', kind: 'quoted', optional: true, placeholder: 'SIP+D2U' },
      { key: 'regexp', label: 'Regexp', kind: 'quoted', optional: true },
      { key: 'replacement', label: 'Replacement', kind: 'host', placeholder: '_sip._udp.example.com.' },
    ],
  },
  {
    type: 'raw',
    label: 'Other type (raw rdata)',
    fields: [
      { key: 'rrtype', label: 'Type', kind: 'enum', placeholder: 'DNAME or TYPE65280', options: [] },
      { key: 'rdata', label: 'Rdata (presentation format)', kind: 'rdata', placeholder: 'target.example.com.' },
    ],
  },
];

export const TYPED = new Set(RECORD_TYPES.map((t) => t.type).filter((t) => t !== 'raw'));

export function typeSpec(type: string): TypeSpec {
  const t = RECORD_TYPES.find((s) => s.type === type);
  return t ?? (RECORD_TYPES[RECORD_TYPES.length - 1] as TypeSpec);
}

export type Values = Record<string, string>;
export type Errors = Record<string, string>;

const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

export function isIPv4(s: string): boolean {
  return IPV4.test(s);
}

export function isIPv6(s: string): boolean {
  if (!s.includes(':') || s.includes('%')) return false;
  let str = s;
  // An embedded IPv4 tail counts as two groups.
  const lastColon = str.lastIndexOf(':');
  const tail = str.slice(lastColon + 1);
  let extra = 0;
  if (tail.includes('.')) {
    if (!isIPv4(tail)) return false;
    str = `${str.slice(0, lastColon + 1)}0`;
    extra = 1;
  }
  const dbl = str.split('::');
  if (dbl.length > 2) return false;
  const groups = (part: string) => (part === '' ? [] : part.split(':'));
  const head = groups(dbl[0] ?? '');
  const rest = dbl.length === 2 ? groups(dbl[1] ?? '') : [];
  const all = [...head, ...rest];
  if (!all.every((g) => /^[0-9a-fA-F]{1,4}$/.test(g))) return false;
  const count = all.length + extra;
  return dbl.length === 2 ? count <= 7 : count === 8;
}

/**
 * A DNS name in presentation form: labels of 1-63 octets, at most 253 in
 * total. Letters, digits, "-" and "_" (service labels); "*" only as the
 * whole first label when wildcard is true. "." alone is the root.
 */
export function hostError(s: string, opts: { wildcard?: boolean; allowRoot?: boolean } = {}): string | null {
  const name = s.trim();
  if (name === '') return 'Required';
  if (name === '.') return opts.allowRoot ? null : 'The root is not allowed here';
  const body = name.endsWith('.') ? name.slice(0, -1) : name;
  if (body.length > 253) return 'Longer than 253 characters';
  const labels = body.split('.');
  for (let i = 0; i < labels.length; i++) {
    const l = labels[i] as string;
    if (l === '') return 'Empty label (two dots in a row)';
    if (l.length > 63) return `Label "${l.slice(0, 12)}…" is longer than 63 characters`;
    if (l === '*') {
      if (i === 0 && opts.wildcard) continue;
      return 'A wildcard is only allowed as the first label';
    }
    if (!/^[A-Za-z0-9_]([A-Za-z0-9_-]*[A-Za-z0-9_])?$/.test(l)) return `Label "${l}" has characters other than letters, digits, - and _`;
  }
  return null;
}

function fieldError(f: FieldSpec, raw: string | undefined): string | null {
  const v = (raw ?? '').trim();
  if (v === '') {
    return f.optional ? null : 'Required';
  }
  switch (f.kind) {
    case 'ipv4':
      return isIPv4(v) ? null : 'Not an IPv4 address';
    case 'ipv6':
      return isIPv6(v) ? null : 'Not an IPv6 address';
    case 'host':
      return hostError(v, { allowRoot: f.key === 'target' || f.key === 'replacement' });
    case 'uint8':
    case 'uint16':
    case 'uint32': {
      if (!/^\d+$/.test(v)) return 'Must be a whole number';
      const n = Number(v);
      const max = f.max ?? (f.kind === 'uint8' ? 255 : f.kind === 'uint16' ? 65535 : 4294967295);
      const min = f.min ?? 0;
      return n < min || n > max ? `Must be between ${min} and ${max}` : null;
    }
    case 'hex':
      return /^([0-9a-fA-F]{2})+$/.test(v.replace(/\s+/g, '')) ? null : 'Must be an even number of hex digits';
    case 'enum':
      if (f.key === 'rrtype') {
        return /^([A-Z][A-Z0-9]*|TYPE\d+)$/.test(v) ? null : 'A type mnemonic (DNAME) or TYPEnnn';
      }
      return f.options && f.options.length && !f.options.includes(v) ? `One of ${f.options.join(', ')}` : null;
    case 'svcparams':
      return v
        .split(/\s+/)
        .every((p) => /^[a-z0-9-]+(=("[^"]*"|[^\s"]+))?$/.test(p))
        ? null
        : 'key=value pairs separated by spaces';
    case 'quoted':
      return v.includes('"') && !(v.startsWith('"') && v.endsWith('"')) ? 'Unbalanced double quote' : null;
    case 'text':
      return /[\r\n]/.test(v) ? 'Must be a single line' : null;
    case 'rdata':
      return /[\r\n]/.test(v) ? 'Must be a single line' : null;
  }
}

/** Per-field errors for one target; empty when valid. */
export function validate(type: string, values: Values): Errors {
  const spec = typeSpec(type);
  const errs: Errors = {};
  for (const f of spec.fields) {
    const e = fieldError(f, values[f.key]);
    if (e) errs[f.key] = e;
  }
  if (spec.type === 'HTTPS' || spec.type === 'SVCB') {
    if (values.priority?.trim() === '0' && values.params?.trim()) {
      errs.params = 'Alias mode (priority 0) takes no parameters';
    }
  }
  return errs;
}

function quote(s: string): string {
  const t = s.trim();
  if (t.startsWith('"') && t.endsWith('"') && t.length >= 2) return t;
  return `"${t.replace(/["\\]/g, (c) => `\\${c}`)}"`;
}

function fqdn(s: string): string {
  const t = s.trim();
  return t === '.' || t.endsWith('.') ? t : `${t}.`;
}

/** Field values → presentation-format target (and the wire type). */
export function compose(type: string, v: Values): { recordType: string; target: string } {
  const g = (k: string) => (v[k] ?? '').trim();
  switch (type) {
    case 'A':
    case 'AAAA':
      return { recordType: type, target: g('address') };
    case 'CNAME':
    case 'PTR':
      return { recordType: type, target: fqdn(g('target')) };
    case 'NS':
      return { recordType: type, target: fqdn(g('host')) };
    case 'MX':
      return { recordType: type, target: `${g('preference')} ${fqdn(g('exchange'))}` };
    case 'SRV':
      return { recordType: type, target: `${g('priority')} ${g('weight')} ${g('port')} ${fqdn(g('target'))}` };
    case 'TXT':
      return { recordType: type, target: v.text ?? '' };
    case 'CAA':
      return { recordType: type, target: `${g('flags')} ${g('tag')} ${quote(g('value'))}` };
    case 'HTTPS':
    case 'SVCB':
      return { recordType: type, target: [g('priority'), fqdn(g('target')), g('params')].filter(Boolean).join(' ') };
    case 'TLSA':
      return { recordType: type, target: `${g('usage')} ${g('selector')} ${g('matching')} ${g('data').replace(/\s+/g, '').toLowerCase()}` };
    case 'SSHFP':
      return { recordType: type, target: `${g('algorithm')} ${g('fptype')} ${g('fingerprint').replace(/\s+/g, '').toLowerCase()}` };
    case 'NAPTR':
      return {
        recordType: type,
        target: `${g('order')} ${g('preference')} ${quote(g('flags'))} ${quote(g('service'))} ${quote(g('regexp'))} ${fqdn(g('replacement'))}`,
      };
    default:
      return { recordType: g('rrtype').toUpperCase(), target: g('rdata') };
  }
}

/** Splits presentation format into tokens, keeping quoted strings whole. */
export function tokenize(s: string): string[] {
  const out: string[] = [];
  const re = /"((?:[^"\\]|\\.)*)"|(\S+)/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    out.push(m[1] !== undefined ? m[1].replace(/\\(["\\])/g, '$1') : (m[2] as string));
  }
  return out;
}

/** Target → field values; unknown or malformed targets fall back to raw. */
export function parse(recordType: string, target: string): { type: string; values: Values } {
  const t = tokenize(target);
  const raw = { type: 'raw', values: { rrtype: recordType, rdata: target } };
  const need = (n: number) => t.length >= n;
  switch (recordType) {
    case 'A':
    case 'AAAA':
      return { type: recordType, values: { address: target.trim() } };
    case 'CNAME':
    case 'PTR':
      return { type: recordType, values: { target: target.trim() } };
    case 'NS':
      return { type: recordType, values: { host: target.trim() } };
    case 'TXT':
      return { type: 'TXT', values: { text: target.startsWith('"') ? t.join('') : target } };
    case 'MX':
      return need(2) ? { type: 'MX', values: { preference: t[0] as string, exchange: t[1] as string } } : raw;
    case 'SRV':
      return need(4)
        ? { type: 'SRV', values: { priority: t[0] as string, weight: t[1] as string, port: t[2] as string, target: t[3] as string } }
        : raw;
    case 'CAA':
      return need(3) ? { type: 'CAA', values: { flags: t[0] as string, tag: t[1] as string, value: t.slice(2).join(' ') } } : raw;
    case 'HTTPS':
    case 'SVCB': {
      if (!need(2)) return raw;
      const parts = target.trim().split(/\s+/);
      return { type: recordType, values: { priority: parts[0] as string, target: parts[1] as string, params: parts.slice(2).join(' ') } };
    }
    case 'TLSA':
      return need(4)
        ? { type: 'TLSA', values: { usage: t[0] as string, selector: t[1] as string, matching: t[2] as string, data: t.slice(3).join('') } }
        : raw;
    case 'SSHFP':
      return need(3)
        ? { type: 'SSHFP', values: { algorithm: t[0] as string, fptype: t[1] as string, fingerprint: t.slice(2).join('') } }
        : raw;
    case 'NAPTR':
      return need(6)
        ? {
            type: 'NAPTR',
            values: {
              order: t[0] as string,
              preference: t[1] as string,
              flags: t[2] as string,
              service: t[3] as string,
              regexp: t[4] as string,
              replacement: t[5] as string,
            },
          }
        : raw;
    default:
      return raw;
  }
}

/** Validates an owner name relative to a zone ("@" is the apex). */
export function ownerError(name: string, zone: string): string | null {
  const n = name.trim();
  if (n === '' || n === '@') return null;
  const err = hostError(n, { wildcard: true });
  if (err) return err;
  if (n.endsWith('.')) {
    const z = zone.endsWith('.') ? zone : `${zone}.`;
    const lower = n.toLowerCase();
    if (lower !== z.toLowerCase() && !lower.endsWith(`.${z.toLowerCase()}`)) return `Not inside ${z}`;
  }
  return null;
}

/** Owner input ("www", "@", "www.example.com.") → absolute dnsName without trailing dot. */
export function ownerToDNSName(name: string, zone: string): string {
  const z = zone.replace(/\.$/, '');
  const n = name.trim();
  if (n === '' || n === '@') return z;
  if (n.endsWith('.')) return n.slice(0, -1);
  return `${n}.${z}`;
}

/** dnsName → owner input relative to the zone. */
export function dnsNameToOwner(dnsName: string, zone: string): string {
  const z = zone.replace(/\.$/, '').toLowerCase();
  const n = dnsName.replace(/\.$/, '');
  if (n.toLowerCase() === z) return '@';
  if (n.toLowerCase().endsWith(`.${z}`)) return n.slice(0, n.length - z.length - 1);
  return `${n}.`;
}
