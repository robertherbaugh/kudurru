// Email clients need tables and inline styles rather than the portal's UI
// components or CSS variables. Brand values and artwork come from branding;
// regenerate brand.json with scripts/mail-brand.py. No external image/font
// requests: the logo is a CID attachment embedded in the server bundle.
import { NONCOMMERCIAL_LICENSE_URL } from '../../shared/catalog';
import brand from './brand.json';
import type { MailMessage } from './types';

const { ink, clay, paper, stone } = brand.colors;
const sans = 'Inter, Arial, Helvetica, sans-serif';
const mono = 'Consolas, Menlo, monospace';
const LOGO_CID = 'kudurru-logo@kudurru.io';

/** The product name in both the plain text signature and HTML footer. */
export const PRODUCT_NAME = 'kudurru license portal';

interface MailPresentation {
  title: string;
  preview: string;
  publicUrl: string;
  action?: { label: string; url: string };
  /** A plain text paragraph rendered as a scannable facts table in HTML. */
  details?: { text: string; heading?: string; rows: readonly (readonly [string, string])[] }[];
  /** A license token, displayed verbatim with wrapping instead of linkification. */
  key?: string;
}

function escape(value: string): string {
  return value.replace(/[&<>"']/g, (ch) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[ch]!);
}

function link(url: string, label = url): string {
  // Presentation URLs come from portal routes, but reject unsafe schemes even
  // when a template is used outside the normal service path.
  if (!/^https?:\/\//i.test(url)) return escape(label);
  return `<a href="${escape(url)}" style="color:${ink};text-decoration:underline;overflow-wrap:anywhere;word-break:break-word;">${escape(label)}</a>`;
}

/** Escape all copy; only explicitly trusted URLs become clickable links. */
function copy(text: string, allowedUrls: readonly string[] = []): string {
  let out = '';
  let at = 0;
  for (const match of text.matchAll(/https?:\/\/[^\s<>"']+/g)) {
    const url = match[0].replace(/[.,;:!?)]+$/, '');
    out += escape(text.slice(at, match.index)) + (allowedUrls.includes(url) ? link(url) : escape(url));
    at = match.index + url.length;
  }
  return (out + escape(text.slice(at))).replace(/\n/g, '<br>');
}

function paragraph(text: string, allowedUrls: readonly string[] = []): string {
  return `<p style="margin:0 0 20px;font-size:15px;line-height:1.7;overflow-wrap:anywhere;word-break:break-word;">${copy(text, allowedUrls)}</p>`;
}

function button(action: NonNullable<MailPresentation['action']>): string {
  if (!/^https?:\/\//i.test(action.url)) return paragraph(action.label);
  return `<table role="presentation" cellpadding="0" cellspacing="0" style="margin:8px 0 24px;"><tr><td bgcolor="${ink}" style="background:${ink};border-radius:4px;mso-padding-alt:14px 22px;">
<a href="${escape(action.url)}" style="display:inline-block;padding:14px 22px;border:1px solid ${ink};border-radius:4px;color:${paper};font-size:15px;font-weight:600;text-decoration:none;">${escape(action.label)} &rarr;</a>
</td></tr></table>`;
}

function details(block: NonNullable<MailPresentation['details']>[number]): string {
  return `${block.heading ? `<h2 style="margin:24px 0 12px;font-size:17px;line-height:1.4;">${escape(block.heading)}</h2>` : ''}
<table cellpadding="0" cellspacing="0" width="100%" style="width:100%;table-layout:fixed;border-top:1px solid ${stone};margin:0 0 24px;">
${block.rows.map(([label, value]) => `<tr><th scope="row" align="left" valign="top" width="30%" style="width:30%;padding:10px 8px 10px 0;border-bottom:1px solid ${stone};font-size:13px;line-height:1.6;font-weight:400;">${escape(label)}</th><td valign="top" style="padding:10px 0;border-bottom:1px solid ${stone};font-size:14px;line-height:1.6;font-weight:600;overflow-wrap:anywhere;word-break:break-word;">${copy(value)}</td></tr>`).join('')}
</table>`;
}

/** Add an HTML alternative while keeping the entire original plain text mail. */
export function brandedMail(message: MailMessage, presentation: MailPresentation): MailMessage {
  const { title, preview, publicUrl, action } = presentation;
  // Each template ends with the product name and portal URL as one paragraph;
  // the HTML footer supplies that same signature.
  const allParts = message.text.trimEnd().split('\n\n');
  if (allParts.at(-1) !== `${PRODUCT_NAME}\n${publicUrl}`) {
    throw new Error('mail text must end with the signature paragraph');
  }
  const parts = allParts.slice(0, -1);
  const allowedUrls = [publicUrl, `${publicUrl}/my/licenses`, `${publicUrl}/requests`, NONCOMMERCIAL_LICENSE_URL, ...(action ? [action.url] : [])];
  const matchedDetails = new Set<NonNullable<MailPresentation['details']>[number]>();
  let keyShown = false;
  let actionShown = false;
  const body = parts.map((part) => {
    const facts = presentation.details?.find((block) => block.text === part);
    if (facts) {
      matchedDetails.add(facts);
      return details(facts);
    }
    if (presentation.key !== undefined && part === presentation.key) {
      keyShown = true;
      return `<p style="margin:0 0 24px;padding:16px;border:1px solid ${stone};font-family:${mono};font-size:12px;line-height:1.7;overflow-wrap:anywhere;word-break:break-all;">${escape(part)}</p>`;
    }
    if (action && part === action.url) {
      actionShown = true;
      return button(action) + paragraph(`Or open this link: ${action.url}`, allowedUrls);
    }
    const rendered = paragraph(part, allowedUrls);
    if (action && part.includes(action.url)) {
      actionShown = true;
      return rendered + button(action);
    }
    return rendered;
  }).join('');
  // A template change must not silently drop a table or the key box. Errors
  // name the broken contract without exposing recipient data or key material.
  if (presentation.details?.some((block) => !matchedDetails.has(block))) {
    throw new Error('mail HTML details must match complete text paragraphs');
  }
  if (presentation.key !== undefined && !keyShown) {
    throw new Error('mail HTML key must match a complete text paragraph');
  }
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light"><title>${escape(message.subject)}</title>
<style>@media only screen and (max-width:620px){.email-shell{width:100%!important}.email-content{padding:28px 20px!important}.email-header{padding:24px 20px!important}.email-title{font-size:28px!important}}</style></head>
<body style="margin:0;padding:0;background:${paper};color:${ink};font-family:${sans};-webkit-text-size-adjust:100%;">
<p aria-hidden="true" style="display:none;max-height:0;overflow:hidden;mso-hide:all;">${escape(preview)}</p>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" bgcolor="${paper}" style="width:100%;background:${paper};"><tr><td align="center" style="padding:24px 12px;">
<!--[if mso]><table role="presentation" width="600" cellpadding="0" cellspacing="0"><tr><td><![endif]-->
<table role="presentation" class="email-shell" width="100%" cellpadding="0" cellspacing="0" style="width:100%;max-width:600px;table-layout:fixed;">
<tr><td class="email-header" bgcolor="${ink}" style="padding:28px 36px 26px;background:${ink};border-bottom:3px solid ${clay};">
<a href="https://kudurru.io" style="text-decoration:none;"><img src="cid:${LOGO_CID}" width="216" height="72" alt="kudurru" style="display:block;width:216px;max-width:100%;height:auto;border:0;color:${paper};font-size:28px;"></a>
<p style="margin:12px 0 0;color:${paper};font-family:${mono};font-size:11px;letter-spacing:1.5px;line-height:1.5;">LICENSE PORTAL</p>
</td></tr>
<tr><td class="email-content" bgcolor="${paper}" style="padding:36px;background:${paper};color:${ink};">
<h1 class="email-title" style="margin:0 0 28px;font-size:32px;line-height:1.2;letter-spacing:-0.8px;font-weight:600;">${escape(title)}</h1>
${body}${action && !actionShown ? button(action) : ''}
</td></tr>
<tr><td style="padding:24px 20px;border-top:1px solid ${stone};color:${ink};font-size:12px;line-height:1.8;text-align:center;">
<p style="margin:0 0 8px;font-family:${mono};">authority over every zone.</p>
<p style="margin:0;">${link(publicUrl, PRODUCT_NAME)} &nbsp;&middot;&nbsp; ${link('https://kudurru.io', 'kudurru.io')}</p>
</td></tr></table>
<!--[if mso]></td></tr></table><![endif]-->
</td></tr></table></body></html>`;
  return {
    ...message,
    html,
    attachments: [...(message.attachments ?? []), {
      filename: 'kudurru-logo.png',
      content: Buffer.from(brand.logo, 'base64'),
      contentType: 'image/png',
      cid: LOGO_CID,
      contentDisposition: 'inline',
    }],
  };
}
