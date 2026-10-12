import { html, type Html } from "../html.ts";
import { LICENSE_CONTACT, LICENSE_FREE_NODES, LICENSE_ROWS, REPO_URL } from "../content.ts";
import { external } from "./layout.ts";

// /license/: what is free, what a key unlocks, how to get and use one.
// The rules in full are LICENSING.md; this page is its short form.
export function license(): Html {
  const mail = (subject: string) => `mailto:${LICENSE_CONTACT}?subject=${encodeURIComponent(subject)}`;
  return html`<section class="sec" aria-labelledby="lic-title">
  <div class="wrap narrow">
    <p class="label">License</p>
    <h1 id="lic-title" class="page-title">Free at home. A key for more.</h1>
    <p class="reg-lead">kudurru is source-available under the PolyForm Noncommercial License 1.0.0. Personal use is free: a homelab, a house, study, a hobby, a charity or a school. A business needs an agreement, including for the parts that need no key.</p>
    <ul class="points">
      <li><strong>DNS never depends on a key.</strong> Queries are answered the same with a key, with none and with an expired one. A key only changes the number of nodes, the number of policies and the features listed below.</li>
      <li><strong>A node without a key is a supported way to run.</strong> One node, two policies, the resolver, the blocking engine, zones, the block page and the dashboard.</li>
      <li><strong>Keys are checked on the node.</strong> A signature check, no license server, no phone home. An expired key keeps working for 30 days.</li>
    </ul>
  </div>
</section>
<section class="sec" aria-labelledby="lic-table-title">
  <div class="wrap">
    <h2 id="lic-table-title">What each license includes</h2>
    <div class="shapes">
      <table>
        <caption class="mono dim">The free personal key covers up to ${LICENSE_FREE_NODES} nodes for non-commercial use.</caption>
        <thead>
          <tr><th scope="col">What</th><th scope="col">No key</th><th scope="col">Personal key (free)</th><th scope="col">Business key</th></tr>
        </thead>
        <tbody>
          ${LICENSE_ROWS.map((r) => html`<tr><th scope="row">${r.what}</th><td>${r.community}</td><td>${r.personal}</td><td>${r.business}</td></tr>`)}
        </tbody>
      </table>
    </div>
  </div>
</section>
<section class="sec" aria-labelledby="lic-get-title">
  <div class="wrap narrow">
    <h2 id="lic-get-title">Get a key</h2>
    <p><strong>A free personal key.</strong> Write to <a href="${mail("Free personal key")}">${LICENSE_CONTACT}</a> with the name to put on it and what you run it for. It covers up to ${LICENSE_FREE_NODES} nodes and is for non-commercial use only.</p>
    <p><strong>A business.</strong> Write to <a href="${mail("kudurru commercial license")}">${LICENSE_CONTACT}</a> with the company, the number of nodes and the features you need. A business key comes with a commercial agreement.</p>
    <h2 id="lic-use-title">Use a key</h2>
    <p>Put the key where the node can read it. The first match wins, and a running node picks up a change within a minute, without a restart.</p>
    <pre class="mono">kudurru serve --license-key &lt;key&gt;
KUDURRU_LICENSE_KEY=&lt;key&gt;
KUDURRU_LICENSE_KEY_FILE=/run/secrets/license_key
&lt;node.dataDir&gt;/license.key</pre>
    <p>In Helm, set <code>license.existingSecret</code> to a Secret with the key; in Compose, set <code>KUDURRU_LICENSE_KEY</code>. <code>kudurru license status</code> shows what a node runs with and the dashboard has a License page.</p>
    <p>The full rules are in ${external(`${REPO_URL}/blob/main/LICENSING.md`, "LICENSING.md")}.</p>
  </div>
</section>`;
}
