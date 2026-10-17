# Kudurru Commercial Software License Agreement

**Version 1.0 — October 12, 2026**

**Not an offer or executed contract until accepted by both parties through an Order Form or other authorized written agreement.**

This Kudurru Commercial Software License Agreement (the **Agreement**) is between **Robert Herbaugh**, at the address stated in the accepted Order Form (**Licensor**), and the person or entity identified as **Customer** on an accepted Order Form. It governs the commercial and government use of the Kudurru DNS software and the applicable source material described below. A person signing for Customer represents that they are authorized to bind that entity.

## 1. Contract documents and precedence

1.1. The Agreement consists of this text, each signed or electronically accepted **Order Form** referencing it, and any separately executed support schedule, service-level agreement, data-processing agreement, or amendment.

1.2. An Order Form controls commercial quantities, fees, dates, and expressly negotiated exceptions. A negotiated signed amendment controls over conflicting standard text. Otherwise, this Agreement controls over a conflicting Order Form's boilerplate or Customer purchase-order terms. Any support or data-processing schedule controls only within its subject matter.

1.3. The **Kudurru Community License** governs uses expressly permitted by it without payment, including qualifying personal, homelab, nonprofit, and community development uses. This Agreement grants additional rights to the identified Customer; it does not eliminate the community rights available to others or revoke lawful grants applicable to earlier software releases.

1.4. A license key or public source repository is a delivery or technical entitlement mechanism, not an independently sufficient commercial license.

## 2. Definitions

- **Software**: Kudurru's DNS server, API, management plane, and associated Kudurru code identified on an Order Form, excluding separately licensed Third-Party Materials.
- **Enterprise Materials**: Licensor-controlled software under the repository's `ee/` directory, including enterprise modules, associated source, build files, and included documentation, to the extent identified as enterprise-licensed by Licensor.
- **Documentation Site Materials**: Licensor-controlled documentation source, themes, configuration, and project-authored content under `docs-site/`, used to create `docs.kudurru.io`; not the public website.
- **Website Materials**: Kudurru's public website code, designs, assets, and content under `website/`, used for `kudurru.io`. These are **excluded** from this Agreement.
- **License Server Materials**: Kudurru's license-issuance service, entitlement-management service, and associated Licensor-controlled source, configurations, and materials under `license-server/`. These are **excluded** from this Agreement.
- **Licensed Materials**: Software, applicable Enterprise Materials expressly included by the Order Form, and Documentation Site Materials only to the extent permitted by Section 3.3 or the Order Form. Website Materials and License Server Materials are not Licensed Materials.
- **Node**: An independently running Kudurru DNS server application instance or replica, whether hosted on a physical host, VM, container, or Kubernetes workload. A replica capable of actively serving or managing DNS is a Node; a stored image, stopped standby, or database backup is not. Separately licensed `dnsdist` or `keepalived` processes are not Nodes merely by existing.
- **Production**: Any live operation delivering DNS services or associated management functions for Customer, Customer personnel, affiliated entities, end users, or a third party, whether publicly exposed, internal-only, revenue-generating, or not.
- **Nonproduction**: Isolated development, build, testing, staging, and disaster-recovery environments not serving routine live DNS traffic. A running hot standby is Production unless the Order Form says otherwise.
- **Authorized Users**: Customer employees, contractors, and agents accessing Licensed Materials on Customer's behalf and subject to confidentiality and use limitations at least as protective as this Agreement.
- **Service Provider Use**: Operating, hosting, supporting, or managing Kudurru for or on behalf of any distinct client or customer, including MSP, MSSP, outsourced IT, DNS hosting, and government contracting.
- **Order Form**: A written or electronically accepted ordering instrument identifying Customer, plan, entitlements, node quantities, permitted service-provider scope, term, price, and any applicable support.
- **Third-Party Materials**: Code, packages, documentation, and assets owned by parties other than Licensor and made available under their own terms.

## 3. Commercial license grant

3.1. **Core commercial rights.** Subject to timely payment and continued compliance, Licensor grants Customer a nonexclusive, nontransferable (except Section 15.4), nonsublicensable, limited license during the Subscription Term to install, execute, use, reproduce as reasonably necessary, and internally modify the Kudurru Software in environments and within Node quantities authorized by the Order Form, including for internal commercial or government Production.

3.2. **Enterprise-feature rights.** If—and only for the time that—the Order Form identifies an **Enterprise** plan or lists specified Enterprise Materials, the same limited rights extend to those Enterprise Materials and feature entitlements. A commercial core-only purchase does not itself unlock every feature under `ee/`.

3.3. **Documentation-site rights.** During the Subscription Term, Customer may access and reference the publicly available Kudurru documentation without restriction imposed by this Agreement and, for internal business use, copy, build, adapt, and host a private mirror of the Documentation Site Materials for its Authorized Users. A mirror must retain required attribution, make clear that any changes are unofficial, and must not be held out as the official `docs.kudurru.io` site. Public republication, external resale, or operation of a competing documentation portal based on Documentation Site Materials requires separate written authorization, except for rights independently granted by applicable law or another license.

3.4. **Modifications.** Customer may modify licensed source for internal use, security hardening, integrations, and interoperability. Customer owns its independently authored modifications, subject to Licensor and contributor rights in the underlying materials. Customer may not publish, sell, sublicense, or redistribute proprietary `ee/` components or non-public commercial modifications except as separately authorized. Contributions voluntarily offered to the public project are governed by its contribution process and any separately accepted Contributor License Agreement (CLA); no ownership transfer occurs merely under this Agreement.

3.5. **Copies for operations.** Customer may make reasonable operational, archival, and backup copies and provide copies to Authorized Users solely to perform permitted operations, subject to written confidentiality obligations. Customer remains responsible for their compliance.

3.6. **Excluded website and license-server source.** This Agreement grants **no right** to reproduce, modify, build, deploy, host, redistribute, sublicense, or otherwise operate source in `website/` or `license-server/`. The separate `website/LICENSE.md` and `license-server/LICENSE.md` govern limited source inspection. Customer's right to receive, install, and locally validate legitimate license keys for Kudurru Software does not include a right to the license-issuance server's source or operations. Viewing the public website and reading public documentation do not require a commercial license.

3.7. **Rights of others.** Rights are granted only to the extent Licensor owns or is authorized to license the relevant material. Copyright holders of third-party contributions retain their rights. Third-Party Materials remain governed by their respective licenses.

## 4. Licensing plans, keys, and node limits

4.1. **Plans.** An Order Form must select **Commercial Core** (commercial Production rights for the core feature set) or **Enterprise** (commercial Production rights plus the Enterprise Materials and named advanced entitlements). Licensor may offer additional plan names or modules only through an accepted Order Form.

4.2. **Counting Nodes.** Each concurrently running Production Node counts toward the purchased Node limit across all covered environments and clusters. Customer may deploy only the total count and organizational or client scope identified on the Order Form; moving a key does not authorize simultaneous use beyond the purchased count.

4.3. **Nonproduction entitlements.** The Order Form controls allowed staging, testing, evaluation, disaster recovery, and failover Nodes. If it is silent, **no additional running Nodes are included beyond the purchased count**. Stored backups, stopped images, and source copies are not running Nodes. Temporary replacement of a failed Node or ordinary rolling upgrade may overlap with the replaced Node for up to **24 hours**, provided the overlap is solely for replacement and never used to increase steady-state capacity.

4.4. **Service providers and affiliates.** Service Provider Use is **not** included unless explicitly authorized on the Order Form. If approved, the Order Form must identify whether Customer licenses a pooled aggregate of Nodes across clients or separate allocations per client; it must define any permitted client entities, reporting requirements, and whether white-labeling is permitted. Affiliate use likewise requires express inclusion in the Order Form. Customer remains the contracting party responsible for license compliance.

4.5. **Government and contractors.** Government entities and government contractors may purchase rights under this Agreement. The Order Form must identify the licensee, permitted operating agencies or contracting scope, and any mandatory statutory clauses in a signed addendum. No public-sector exemption is inferred from nonprofit, research, education, or public-purpose status.

4.6. **Key enforcement.** Keys may encode plan, customer, feature, validity, and deployment or Node entitlements and be verified locally. Technical enforcement is not a waiver of contractual conditions. Customer may not manipulate or bypass license enforcement to exceed purchased entitlements. This restriction does not prohibit permitted security research, accessibility, or other acts protected by applicable law.

4.7. **DNS continuity.** The Kudurru design aims to preserve existing DNS answering if a key is absent, invalid, or expired; management-plane functions or feature activation may become limited. The current documented technical design includes a **30-day key-expiry grace period**. These operational safeguards **do not extend the contractual Subscription Term or grant unpaid commercial rights**. Customer remains responsible for legal operation and renewal; there is no contractual guarantee of uninterrupted DNS availability unless separately agreed.

## 5. Fees, ordering, and payment

5.1. Customer will pay the subscription fees, Node fees, additional feature fees, and any support charges stated in the Order Form. No public list price becomes part of this Agreement unless expressly incorporated.

5.2. Unless the Order Form states otherwise, fees are invoiced annually in advance, due within **30 calendar days** of invoice, in **USD**, and are nonrefundable except where this Agreement expressly provides. Changes to Node quantities or plan level require an accepted amendment or additional Order Form and may incur prorated charges.

5.3. Fees exclude applicable sales, use, and similar indirect taxes, which Customer will pay except taxes on Licensor's net income. Neither party is required to waive applicable tax exemptions.

5.4. If an undisputed payment is over 15 days late, Licensor may suspend issuance or renewal of commercial keys after giving at least 15 days' written notice and an opportunity to pay. Licensor will not intentionally command Kudurru to stop responding to DNS queries as a collection method. Suspension does not relieve Customer of contractual obligations.

5.5. Customer must dispute an invoice in writing, with a reasonable explanation, before its due date; parties will work in good faith to resolve it. Undisputed portions remain payable.

## 6. Customer obligations and prohibited acts

6.1. Customer will safeguard access credentials and commercial keys, restrict source access to Authorized Users, and maintain reasonable security controls appropriate to DNS infrastructure.

6.2. Except as expressly authorized by this Agreement, the Order Form, applicable third-party licenses, or nonwaivable law, Customer must not: (a) distribute, publish, resell, sublicense, or offer the Licensed Materials as a standalone product; (b) operate for third parties without Service Provider Use rights; (c) use more Nodes or higher-tier features than purchased; (d) remove legal notices; (e) falsely claim ownership, official project status, or endorsement; or (f) publish proprietary source from `ee/` or full unauthorized copies of `docs-site/`. The separate source-viewing notices for `website/` and `license-server/` control those excluded materials.

6.3. Customer may publish interoperable software independently created by it and may report security vulnerabilities in good faith. This Agreement does not prohibit legitimate benchmarking, truthful public reviews, or security research, subject to confidentiality obligations covering genuinely nonpublic materials.

6.4. Neither party's terms override mandatory legal rights or licenses governing separable Third-Party Materials.

## 7. Support, updates, and services

7.1. Availability of maintenance updates, upgrades, vulnerability fixes, technical support, professional services, response times, or uptime guarantees depends exclusively on the Order Form and any incorporated support schedule. No support SLA is included by default.

7.2. Customer is responsible for infrastructure, backups, DNS zone data, key rotation, routing, resilience, and operational testing unless purchased services expressly assign those responsibilities to Licensor.

7.3. Licensor may release updates under new community terms for future versions, but Customer's commercial rights to the specific versions made available under its valid Subscription Term remain subject to this Agreement, the Order Form, and applicable change-of-license commitments, if any.

## 8. Ownership and feedback

8.1. Licensor and applicable rightsholders retain all intellectual property rights in the underlying Licensed Materials. Customer receives only the express licenses stated in this Agreement. No trademark, trade dress, trade name, or domain rights are conveyed except to identify Kudurru accurately as a product Customer uses.

8.2. Customer may provide optional feedback; unless the parties agree otherwise in writing, Customer grants Licensor a nonexclusive, perpetual, worldwide, royalty-free license to use and implement nonconfidential feedback without obligation. Source-code contributions are excluded from this clause and require the applicable separate contribution terms.

8.3. Customer retains ownership of its DNS zones, configuration data, customer records, and other Customer-provided data. Licensor receives no ownership of that data by supplying licensed software.

## 9. Confidentiality

9.1. **Confidential Information** means nonpublic business, technical, security, pricing, or product information marked confidential or that a reasonable person would understand to be confidential, including nonpublic commercial source, private keys, and customer records. Public source that Licensor voluntarily publishes is not confidential merely because it is source-available, although its copyright/license restrictions still apply.

9.2. Each party will use the other's Confidential Information only to perform this Agreement; protect it using reasonable care (not less than it protects its own similar information); and disclose it only to personnel and professional advisers with a need to know and appropriate obligations.

9.3. Exclusions cover information independently developed, properly obtained without restriction, public without breach, or already lawfully known. Legally compelled disclosure is permitted with prior notice when lawful.

9.4. On termination, each party will return or delete the other's Confidential Information on reasonable request, except legal retention, immutable backups pending deletion, and evidence needed to enforce rights.

## 10. Records and verification

10.1. Customer will maintain accurate records of purchased and deployed Production Node quantities, authorized affiliates and clients, and applicable use for at least **two years** after the relevant Subscription Term.

10.2. No more than once in any 12-month period, Licensor may request a written self-certification of Node usage and applicable client scope on **15 business days' notice**. If reasonable documented evidence of material underlicensing arises, Licensor may request relevant limited records or a mutually agreed independent audit, under confidentiality, during normal business hours and not more than necessary to verify compliance.

10.3. If verified underlicensing exists, Customer will promptly purchase the missing entitlements and pay accrued fees at the applicable agreed or then-current rates as reasonably determined under the Order Form. Audit costs shift to Customer only for material underlicensing exceeding **10%** of purchased Production Nodes; otherwise Licensor bears them. These remedies do not authorize invasive production scans or disclosure of client DNS query logs.

## 11. Warranties and disclaimers

11.1. Each party warrants that it has authority to enter into this Agreement. Licensor warrants that it has the right to grant the expressly stated rights to Licensor-controlled materials.

11.2. **EXCEPT FOR THE EXPRESS WARRANTIES ABOVE OR ANY SEPARATE SIGNED SUPPORT COMMITMENT, LICENSED MATERIALS ARE PROVIDED “AS IS,” TO THE MAXIMUM EXTENT PERMITTED BY LAW, WITHOUT WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, NON-INFRINGEMENT, OR UNINTERRUPTED OR ERROR-FREE OPERATION.** No statement concerning license enforcement or DNS continuity constitutes a production availability warranty.

11.3. Each party will comply with laws applicable to its obligations. This provision does not expand the rights granted under third-party licenses or override mandatory government contracting requirements.

## 12. Liability allocation

12.1. **TO THE MAXIMUM EXTENT PERMITTED BY LAW, NEITHER PARTY WILL BE LIABLE FOR INDIRECT, INCIDENTAL, SPECIAL, CONSEQUENTIAL, EXEMPLARY, OR PUNITIVE DAMAGES, INCLUDING LOST PROFITS OR LOST BUSINESS, EVEN IF ADVISED OF THEIR POSSIBILITY.**

12.2. **EACH PARTY'S TOTAL LIABILITY ARISING FROM THIS AGREEMENT WILL NOT EXCEED THE FEES PAID OR PAYABLE BY CUSTOMER UNDER THIS AGREEMENT DURING THE TWELVE MONTHS BEFORE THE EVENT GIVING RISE TO THE CLAIM.** This limitation does not limit (a) Customer's payment obligations, (b) either party's fraud or willful misconduct, or (c) liability that cannot legally be limited.

12.3. Nothing excludes liability that applicable law prohibits excluding.

## 13. Term, nonrenewal, and termination

13.1. The Agreement begins on the Effective Date stated in the first accepted Order Form. Each Subscription Term begins and ends as stated in its Order Form. **No automatic renewal occurs unless expressly specified in the Order Form.** Renewal pricing and entitlements require an accepted renewal Order Form or other authorized agreement.

13.2. Either party may terminate an affected Order Form for material breach not cured within **30 days** after written notice. A material license-scope violation or confidentiality misuse may justify immediate suspension of the affected grant to the extent necessary to prevent continuing infringement, subject to applicable law.

13.3. Upon expiration or termination, Customer must cease commercial use of the affected Licensed Materials and stop deploying additional Nodes, except to the extent separately licensed. Within **30 days**, Customer will uninstall or disable unlicensed commercial deployments, remove protected source copies (subject to permitted legal or backup retention), and certify completion on reasonable request. Any transition period requires a separate written agreement.

13.4. Licensor will not intentionally use key revocation to remotely terminate ongoing DNS answering. **Technical service continuity does not extend Customer's legal right to use the software after license expiration.** Customer must plan migration or renewal in advance.

13.5. Sections concerning accrued payments, ownership, confidentiality, restrictions on unlicensed distribution, verification of historical compliance, warranty disclaimers, liability limitations, and disputes survive to the extent needed to enforce them.

## 14. Third-party materials and historical releases

14.1. Separately licensed dependencies, tools, packages, fonts, datasets, and assets remain governed by their own terms, which take precedence for the relevant components. For example, separately distributed `dnsdist` and `keepalived` remain subject to their applicable GPL terms; materials specifically marked Apache-2.0 retain those grants.

14.2. This Agreement does not revoke or narrow Apache-2.0, PolyForm Noncommercial, or other permissions validly granted for historical Kudurru versions or third-party contributions. A prior permissively licensed release may remain independently usable under its original terms.

14.3. Where a third-party contributor has not granted Licensor sufficient commercial sublicensing rights, Licensor cannot grant more rights than it possesses; the affected contribution's own license controls.

## 15. General provisions

15.1. **Notices.** Contract notices must be sent to the email or postal contact stated in the Order Form and are effective upon confirmed delivery, except where governing law mandates another method. Licensing inquiries: **hello@kudurru.io**.

15.2. **Law and venue.** This Agreement is governed by the laws of the jurisdiction stated in the accepted Order Form, without conflict-of-law rules, and disputes will be resolved in the courts and venue stated there, subject to mandatory law and government procurement requirements.

15.3. **Force majeure.** Neither party is liable for nonpayment-independent failures caused by events beyond reasonable control if it gives timely notice and uses reasonable mitigation; accrued payment obligations remain due.

15.4. **Assignment.** Neither party may assign this Agreement without the other's prior written consent, not unreasonably withheld, except to a successor in a merger or sale of substantially all assets that assumes all obligations and is not a direct competitor of the nonassigning party; any such assignment remains subject to applicable law and governmental transfer restrictions.

15.5. **Entire agreement; severability; waiver.** This Agreement and accepted Order Forms are the entire agreement concerning their subject matter. Amendments require both parties' authorized written acceptance. Invalid provisions will be narrowed where permissible without invalidating the remainder. Failure to enforce a term once is not a waiver.

15.6. **Independent parties.** Neither party is the other's employee, partner, agent, or joint venturer. No third party obtains rights under this Agreement except where expressly required by law or a signed government schedule.

---

# Schedule A — Order Form Template

**Order Form No.:** [NUMBER]

**Effective Date:** [DATE]

**Licensor:** Robert Herbaugh / [ADDRESS / BILLING CONTACT]

**Customer:** [LEGAL NAME / ADDRESS / BILLING CONTACT]

**Authorized contacts:** [EMAILS]

| Field | Agreed terms |
|---|---|
| License plan | [ ] Commercial Core  [ ] Enterprise |
| Included Enterprise modules | [ALL CURRENT `ee/` MODULES / NAMED MODULES / NONE] |
| Licensed Production Nodes | [NUMBER] concurrent running Nodes across [ENVIRONMENTS] |
| Permitted clusters / entities | [IDENTIFY OR “ALL CUSTOMER-OWNED, SUBJECT TO NODE CAP”] |
| Nonproduction / standby Nodes | [NUMBER AND WHETHER RUNNING DR IS COUNTED] |
| Service Provider Use | [NOT AUTHORIZED / AUTHORIZED; CLIENTS OR POOLED SCOPE] |
| Government use | [NO / YES; AGENCIES, PROCUREMENT ADDENDUM] |
| Internal documentation mirror | Included for Authorized Users under Section 3.3 |
| Website and license-server source | Excluded (`website/` and `license-server/`); no deployment or modification rights |
| Subscription Start / End | [DATES] |
| Annual fees / Node fees | [CURRENCY, PRICE AND PAYMENT SCHEDULE] |
| License key policy | [DELIVERY, OFFLINE USE, DEPLOYMENT ID IF APPLICABLE] |
| Support / SLAs | [NONE / SCHEDULE REFERENCE] |
| Renewal | [EXPLICIT RENEWAL TERMS; NONE BY DEFAULT] |
| Governing law and venue | [JURISDICTION / COURTS AND VENUE] |
| Special terms | [ANY EXPRESS OVERRIDES] |

**Customer acceptance:**

Authorized signatory: ____________________  Date: __________

**Licensor acceptance:**

Authorized signatory: ____________________  Date: __________
