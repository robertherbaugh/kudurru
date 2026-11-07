# Enterprise custom branding

In v0.2, an Enterprise license can customize the dashboard, sign-in page and
block page. Personal, Business and Trial keys use the built-in kudurru brand,
including keys with an unrestricted feature list. An Enterprise key with a
restricted feature list must include `branding`.

Open **Settings → Branding** as an administrator with `settings.write`:

1. Enter an organization name (up to 80 characters).
2. Upload light and dark PNG logos. One uploaded logo can serve both modes.
3. Optionally upload a PNG browser favicon and choose a `#RRGGBB` accent color.
4. Save. Changes are cluster settings and become visible after publication.

Images must be static PNG files, at most 64 KiB each. Logos can be up to
1024 × 1024 pixels and favicons up to 256 × 256 pixels. The editor preserves
aspect ratios. Remote image URLs, SVG, animated PNG and custom CSS are not
supported. Logo files stay in the cluster configuration; browsers do not fetch
them from a third party. Choose artwork readable on both light and dark surfaces.

The accent derives readable text and interaction colors from your chosen color.
It preserves each user's selected theme, color mode and contrast setting, and
keeps DNS status colors unchanged. The favicon applies to browser tabs; installed
application manifests and apple-touch icons retain the shipped identity.

Empty fields restore their defaults. File/environment-managed settings remain
read-only where applicable. The API uses the same cluster settings route:

```json
{
  "branding": {
    "name": "Example Network",
    "accentColor": "#2457A6"
  }
}
```

Send this JSON to `PATCH /api/v1/settings` with an authorized credential. Image
settings `logoLight`, `logoDark` and `favicon` accept canonical
`data:image/png;base64,...` values within the same limits. No restart is required.
On SQLite the current primary admits the write; followers do not forward it yet.

The existing technical grace period applies. When Enterprise entitlement ends,
the default kudurru presentation returns and saved branding remains available
for reactivation. A missing/unavailable public branding response also falls back
to defaults. Dashboard clients recheck periodically and on focus, and stop using
an enabled response at its original deadline. License changes never interrupt
DNS answering or the basic block page.
