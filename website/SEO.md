# Search launch and measurement

The production site is `https://kudurru.io/`. Search titles, descriptions,
social cards, JSON-LD, canonical URLs, robots and the sitemap are generated
by `build.ts`. Run `make website-check` before publishing changes.

## Pages and reader intent

| Page | Reader's question | Next step |
| --- | --- | --- |
| `/` | What is kudurru? | Explore the dashboard or choose a setup guide |
| `/kubernetes-dns/` | Can I manage DNS zones and filtering in Kubernetes? | Kubernetes quick start and Helm guide |
| `/network-ad-blocking/` | How can I filter ad and tracker domains on my network? | Home network and Docker setup |
| `/pi-hole-alternative/` | Would kudurru fit my existing Pi-hole setup? | Manual migration guide, capabilities and limitations |
| `/enterprise/` | Can my organization run kudurru? | Enterprise information and contact |
| `/license/` | Which license and key do I need? | License details and key request |

Titles describe the task the reader wants to accomplish. Keep capabilities
and limitations accurate. The pages do not claim automatic migration,
DHCPv6 support, DNSSEC validation of forwarded answers (only recursive
upstream groups validate), or Pi-hole API compatibility.
DHCPv4 is included in v0.1.0, disabled by default, on Linux/SQLite, with
dashboard setup and optional three-node quorum failover.
Schema data does not infer an offer or a zero price from free eligible use.
Coordinate pricing changes with the enterprise and license content.

## Before launch

1. Deploy the checked build through the existing release process. Production
   must not have `WWW_NOINDEX=1`. Set that variable for non-production
   builds if their preview URLs can be discovered; verify their response
   has `X-Robots-Tag: noindex`. Cloudflare builds this website from `release`,
   as described in [README.md](README.md#deploying).
2. Check DNS and permanent redirects for `www.kudurru.io`, `kudurru.app`,
   `www.kudurru.app`, `kudurru.dev` and `www.kudurru.dev`. They should
   resolve and redirect to the same path and query on `https://kudurru.io`
   with status 301. Also check HTTP redirects to HTTPS. The redirect
   configuration lives in Cloudflare, outside this repository; follow
   [the alias setup](README.md#deploying).
3. Fetch `/robots.txt` and `/sitemap.xml`: both should return 200, and the
   production sitemap should contain the new landing pages. Check an
   unknown path returns 404 rather than the home page. Inspect response
   headers to confirm production pages have no `noindex` directive.
4. Check each landing page with JavaScript disabled. The text, links and
   next step should remain available. Check a phone-sized viewport for
   horizontal overflow and readable text.
5. Validate the home page's JSON-LD with the
   [Schema Markup Validator](https://validator.schema.org/) and inspect the
   rendered HTML in Search Console after deployment. Software markup here
   describes the application; without qualifying offers or ratings it is
   not intended to satisfy Google's software rich-result requirements.

Initial audit on October 15, 2026: the apex home page, robots, sitemap and
social image returned 200, and an unknown path returned 404.
`www.kudurru.io` did not resolve from the build environment. Check public
DNS and provision its proxied record and Bulk Redirect before promoting
that alias. Recheck all aliases after configuration changes.

## Search engine accounts

1. In [Google Search Console](https://search.google.com/search-console/),
   add a **Domain property** for `kudurru.io` and verify ownership using
   the DNS TXT record Google supplies in the Cloudflare DNS zone. This
   covers the apex and documentation subdomain without putting a
   verification token in the website source.
2. Submit `https://kudurru.io/sitemap.xml`. Submit the documentation site's
   own `https://docs.kudurru.io/sitemap.xml` separately after checking that
   it returns the current documentation URLs.
3. Use URL Inspection on the home page and each new landing page. Check
   crawl availability, rendered content and the selected canonical. Request
   indexing after the production release. Submission is a discovery
   signal; search engines decide when to crawl and index.
4. In [Bing Webmaster Tools](https://www.bing.com/webmasters/), import the
   verified Search Console property or verify the domain with Bing's DNS
   record, then submit the same sitemaps.

These account actions are performed by a domain owner. There are no search
engine credentials or ownership tokens in this repository.

## Measure and improve

Review Search Console weekly once data appears. Filter the Performance
report by each landing-page URL to compare impressions, clicks,
click-through rate and search queries. Track Kubernetes DNS, DNS ad
blocking and Pi-hole migration queries separately. Use the Pages report
to investigate unexpected exclusions and Core Web Vitals to investigate
real user loading or layout problems.

Choose follow-up content from actual queries and recurring user questions.
Update a page when its search promise differs from what readers find,
and link to the relevant guide from release notes or useful community
answers when appropriate. Avoid creating many similar pages for spelling
variations of the same query.

Search Console measures search visits; it does not measure whether someone
installed kudurru. The calls to action lead to the setup and migration
guides. To measure those clicks, first choose an analytics service and
decide which events to collect; this implementation adds no tracking
scripts or third-party requests.
