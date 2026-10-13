# AGENTS

Project-specific guidance for AI coding agents.

<!-- ASTRYX:START -->
Astryx v0.6.8 · 168 components
CLI: run every command as `bunx astryx <cmd>` (shown below as `astryx ...`).

SETUP (once, in app entry e.g. main.tsx) — without these, components render unstyled:
  import "@astryxdesign/core/reset.css";
  import "@astryxdesign/core/astryx.css";

WORKFLOW — start every page from a template, never from scratch:
1. `astryx build "<idea>"` — names the template to scaffold from and the parts it lacks.
2. `astryx template <name> <path>` — scaffold it; keep its frame, gap and padding; replace the content.
3. `astryx template <Block>` for parts it lacks; `astryx component <Name>` to read props before using one.
Changing an existing page? Keep it: skip step 2 and add blocks/components inside its sections.

RULES:
- No <div> — components handle all layout and spacing.
- Read `astryx docs layout` before changing a template's frame.
- Dense data = rows (Table, List/Item), not Card-wrapped lists. Badge = counts only.
- Style with component props first, then tokens (var(--color-*|--spacing-*|--radius-*)). No raw hex/px. No StyleX/Tailwind compiler here — don't use xstyle/utility classes directly. Some templates use StyleX and need a compiler plugin; see `astryx docs styling-overview`.
- Palettes and custom colors go through the theme, never :root overrides. See `astryx docs theme`.
- SELF-CHECK: re-read the file; replace any raw <div>/<span>, imported .css/@apply, or hardcoded #hex/px with the component or a token.

`astryx help` lists every command. Key ones beyond the workflow:
  search "<query>"   find any component / hook / doc / template
  discover <words>   integrations you could add
  docs <topic>       getting-started, principles, tokens, theme … (`astryx docs` lists all)
  docs cli           commands, API reference, integration authoring
  upgrade --from <old version> --apply   run after a dependency bump
<!-- ASTRYX:END -->

## license-server

- Read [README.md](README.md) (what it is, configuration, roles, keys) and
  [docs/architecture.md](docs/architecture.md) (the flows) first. The design
  record is docs/architecture.md D165 in the repository root.
- Hard rules: no kudurru node ever contacts the portal; keys must stay
  byte-identical to `internal/license` (`server/test/jws.test.ts`, the Go
  golden vectors, the node oracle); never `features: []`, never `nbf`; the
  signing key never reaches the database, the image or a log line.
- `shared/routes.ts` is the one list of routes and their access rule; add a
  route there, give it a handler, and the RBAC test covers it. Wire types
  live in `shared/api.ts` only.
- UI (`ui/`): the rules below and docs/m3/ui.md ("Pages, wizards and
  dialogs") apply as in `web/`. Look up every component with
  `bun run astryx -- component <Name> --dense` (run from license-server/).
  `ui/src/ui/` is copied from `web/src`: each file names its source; keep
  them in step by hand.
- A new table or a new kind of query needs its grant in a new migration
  (like `0002_runtime_grants.sql`); `server/test/roles.test.ts` fails
  otherwise.
- Checks: `bun run typecheck && bun run lint && bun run test && bun run build`
  (database tests: `TEST_DATABASE_URL`, `REQUIRE_DB=1`; Go checks:
  `REQUIRE_GO=1`), and `bunx playwright test` against `compose.yaml`.
