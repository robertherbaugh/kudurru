import { html, type Html } from "../html.ts";
import { BUSINESS_SUBJECT, LICENSE_CONTACT, LICENSE_FREE_NODES, LICENSE_REQUIRED_NOTICE, LICENSE_ROWS, LICENSE_TEXT_FILE, LICENSE_TEXT_PATH, LICENSE_SOURCE_URL, REPO_URL, docs } from "../content.ts";
import { external } from "./layout.ts";
import type { Rendered } from "../markdown.ts";

// /license/: what is free, what a key unlocks and how to get one. Using a
// key on a node is in the docs (LICENSING.md, the admin guide).
// The rules in full are LICENSING.md; this page is its short form.
const REQUEST_PATH = "/api/v1/public/key-requests"; // the license server's public form endpoint

// The form: a personal request goes to the license server, anything else
// opens the visitor's mail program (src/client/license.ts). It needs script,
// so without script the two mail links of the "Get a key" text stand in.
// It is the first row of its section: the heading in the first column, the
// fields in a box of form width in the second.
function requestForm(licenseApi: string): Html {
  const endpoint = new URL(REQUEST_PATH, licenseApi).toString();
  return html`<form class="licform lic-row" data-license-form data-endpoint="${endpoint}" novalidate aria-labelledby="lic-req-title">
      <h2 id="lic-req-title">Request a key</h2>
      <div class="licform-box">
        <fieldset class="licform-kind">
          <legend>What is it for?</legend>
          <label class="licform-choice"><input type="radio" name="kind" value="personal" checked> <span>Eligible personal, homelab, nonprofit or community-development use under the Community License. Free, up to ${LICENSE_FREE_NODES} nodes.</span></label>
          <label class="licform-choice"><input type="radio" name="kind" value="business"> <span>Business or government production, an MSP, or anything else. We reply by email.</span></label>
        </fieldset>
        <div class="licform-grid">
          <label class="licform-field"><span>Your name</span><input type="text" name="name" autocomplete="name" maxlength="200" required></label>
          <label class="licform-field"><span>Email address</span><input type="email" name="email" autocomplete="email" maxlength="254" required></label>
        </div>
        <div data-panel="personal">
          <label class="licform-field"><span>What do you run kudurru for?</span><textarea name="personal-use" rows="3" maxlength="1000" required></textarea></label>
          <label class="licform-check"><input type="checkbox" name="noncommercial"> <span>I agree and certify this key will only be used for eligible personal, qualifying nonprofit or community-development use in accordance with the license (<a href="${LICENSE_TEXT_PATH}" target="_blank" rel="noopener">Kudurru Community License</a>).</span></label>
        </div>
        <div data-panel="business" hidden>
          <div class="licform-grid">
            <label class="licform-field"><span>Business or organization</span><input type="text" name="company" autocomplete="organization" maxlength="160" required></label>
            <label class="licform-field"><span>Number of nodes</span><input type="number" name="business-nodes" min="1" max="10000" inputmode="numeric" required></label>
          </div>
          <label class="licform-field"><span>Details (the features you need, your timeline; optional)</span><textarea name="business-use" rows="3" maxlength="1500"></textarea></label>
        </div>
        <div class="licform-trap" aria-hidden="true"><label>Leave this empty <input type="text" name="website" tabindex="-1" autocomplete="off"></label></div>
        <div class="licform-problems" data-problems-box hidden><ul data-problems></ul></div>
        <div class="actions"><button class="btn btn-primary" type="submit">Request a key</button></div>
        <p class="licform-status" data-status role="status" aria-live="polite"></p>
        <div class="licform-mail" data-mail hidden>
          <pre class="mono" id="lic-mail-text" data-mail-text></pre>
          <button class="more" type="button" data-copy="lic-mail-text"><span>Copy</span></button>
        </div>
        <p class="fine">A personal request goes to our license server, which emails you a link to confirm your address. After you confirm, we review the request and email your key once it is approved. A business request is not sent anywhere: it opens an email to ${LICENSE_CONTACT} with the subject ${BUSINESS_SUBJECT} and your details filled in.</p>
      </div>
    </form>`;
}

// The intro sits in the narrow column, as on /license/text/. The sections
// below it span the full width, as the table needs: paragraphs keep their
// measure and only the form's box is narrower (site.css, "The license page").
export function license(licenseApi: string): Html {
  const mail = (subject: string) => `mailto:${LICENSE_CONTACT}?subject=${encodeURIComponent(subject)}`;
  return html`<section class="sec" aria-labelledby="lic-title">
  <div class="wrap narrow">
    <p class="label">License</p>
    <h1 id="lic-title" class="page-title">Free at home. A key for more.</h1>
    <p class="reg-lead">The core DNS server and dashboard use the Kudurru Community License. Personal, household, homelab and qualifying nonprofit use is free under its terms. Isolated development and evaluation are permitted, including for businesses and governments. Business and government production use needs an accepted commercial agreement and Order Form, including for the parts that need no key.</p>
    <ul class="points">
      <li><strong>DNS never depends on a key.</strong> Queries are answered the same with a key, with none and with an expired one. A key only changes the number of nodes, the number of policies and the features listed below.</li>
      <li><strong>A node without a key is a supported way to run.</strong> One node, the default policy, the resolver, the blocking engine, zones, the block page and the dashboard.</li>
      <li><strong>Keys are checked on the node.</strong> A signature check, no license server, no phone home. An expired key keeps working for 30 days.</li>
      <li><strong>A free key is for eligible community use only.</strong> It unlocks features; it does not grant business or government production rights or extend an expired agreement.</li>
    </ul>
  </div>
</section>
<section class="sec lic-sec" aria-labelledby="lic-table-title">
  <div class="wrap">
    <h2 id="lic-table-title">What each key unlocks</h2>
    <div class="shapes lic-table">
      <table>
        <caption class="mono dim">The free personal/nonprofit key covers up to ${LICENSE_FREE_NODES} nodes for eligible community use.</caption>
        <thead>
          <tr><th scope="col">What</th><th scope="col">No key</th><th scope="col">Personal/nonprofit key (free)</th><th scope="col">Business key</th></tr>
        </thead>
        <tbody>
          ${LICENSE_ROWS.map((r) => html`<tr><th scope="row">${r.what}</th><td>${r.community}</td><td>${r.personal}</td><td>${r.business}</td></tr>`)}
        </tbody>
      </table>
    </div>
    <p class="fine">* If included in commercial license agreement.</p>
  </div>
</section>
<section class="sec lic-sec" aria-labelledby="lic-get-title">
  <div class="wrap">
    ${requestForm(licenseApi)}
    <div class="lic-row">
      <h2 id="lic-get-title">Get a key</h2>
      <div class="lic-body">
        <p class="nojs-note">The form needs JavaScript. Without it, write to us:</p>
        <p><strong>A free personal/nonprofit key.</strong> Write to <a href="${mail("Free personal key")}">${LICENSE_CONTACT}</a> with the name to put on it and what you run it for. It covers up to ${LICENSE_FREE_NODES} nodes for eligible community use. Schools and research organizations must meet the Community License's nonprofit eligibility rules.</p>
        <p><strong>A business, government or MSP.</strong> Write to <a href="${mail("kudurru commercial license")}">${LICENSE_CONTACT}</a> with the organization, the number of nodes and the features you need. The ${external(docs("/project/commercial-license/"), "commercial agreement")} requires an accepted Order Form. A Commercial Core purchase does not automatically include enterprise modules; those must be authorized by the Order Form.</p>
      </div>
    </div>
    <div class="lic-row">
      <h2 id="lic-read-title">Read the license</h2>
      <div class="lic-body">
        <p>The license is the <a href="${LICENSE_TEXT_PATH}">Kudurru Community License</a>; the full text is on this site. The rules for people, what a key unlocks and what a missing key changes, are in the ${external(docs("/project/licensing/"), "licensing page of the docs")}, and the docs carry the ${external(docs("/project/license/"), "license text")} too. The source is in ${external(REPO_URL, "the repository")} (<code>LICENSE</code> and <code>LICENSING.md</code>).</p>
        <p>Community modifications and shared copies must retain notices, include the license and identify changes; distributed executables with changes must include access to the corresponding source. The folder notices govern ${external(docs("/project/enterprise-license/"), "enterprise materials")} and ${external(docs("/project/documentation-license/"), "documentation source")}. Anyone may read the published docs.</p>
        <p>The ${external(docs("/project/website-license/"), "marketing website")} and ${external(docs("/project/license-server-license/"), "license server")} have separate source-viewing licenses. Inspection is permitted; modification, redistribution and hosting require prior written authorization. Neither the Community License nor a commercial purchase grants those rights.</p>
      </div>
    </div>
  </div>
</section>`;
}

// /license/text/: the license itself, rendered from the repository's LICENSE
// file at build time (src/markdown.ts), so the site, the docs and the
// repository show one text.
export function licenseText(r: Rendered): Html {
  return html`<section class="sec" aria-labelledby="lic-text-title">
  <div class="wrap narrow">
    <p class="label">License</p>
    <h1 id="lic-text-title" class="page-title">${r.title}</h1>
    <p class="reg-lead">This is the license of kudurru. It is the text of the ${external(LICENSE_SOURCE_URL, "Kudurru Community License")}, unchanged, and the same as the <code>LICENSE</code> file of the repository. <a href="${LICENSE_TEXT_FILE}">Download it as text</a>. What it means for you, and how to get a key, is on <a href="/license/">License and keys</a>.</p>
    <div class="license-text">${r.body}</div>
    <p class="fine mono">${LICENSE_REQUIRED_NOTICE}</p>
  </div>
</section>`;
}
