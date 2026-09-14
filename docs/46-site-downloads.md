# R46 — Download section on the project site (docs/46)

**Status**: designed and implemented 2026-09-14. Chunks **DL1–DL5** (`DL` =
DownLoads; two-letter prefix per the R21+ convention). Site + CI, plus two
footer links in the SPA's landing page (DL5, §6): no relay, wire or
broadcaster code moves.

## 1. Purpose

The native broadcasters are the only part of gawk a user has to *download*,
and until now the project site (`site/`, published to
https://tuhis.github.io/gawk/) did not offer them. It mentioned "native apps
for Linux and Windows" in a paragraph under the browser-support table and
linked to a design doc. Getting the binary meant knowing that the releases
page holds six components' worth of tags, finding the newest
`gawk-broadcast/…` or `gawk-broadcast-windows/…` among them, and picking
the asset.

This milestone adds a **Download** section to the landing page with one
card per platform: the newest version, its date and size, a direct
download button, a one-line install hint and the SHA-256 of the file. The
section keeps itself current: nothing on the site is edited when a release
ships.

## 2. Decisions

**D1 — The version source is a static per-component manifest on the
`badges` branch, not the GitHub API.** `releases/latest` is meaningless in
a monorepo whose six components tag separately — it names whichever release
was created last, which on a combined release PR is `gawk-admin`. The
releases list endpoint is rate-limited per visitor IP unauthenticated
(60/hour), so a page that called it would break for anyone behind a shared
NAT during a busy hour. Instead the job that attaches the binaries also
writes `releases/<component>/latest.json` to the orphan `badges` branch that
R41 already publishes shields.io endpoints to (docs/43 D1): same token, same
publish path, served by raw.githubusercontent.com over TLS with permissive
CORS and no quota. **This is the manifest R45 sketched for the desktop
update check**, shipped early; R45 phase 1 now needs only the client half.

**D2 — Written only after a successful attach.** The manifest is produced
by `.github/actions/publish-release-manifest`, run by the attach jobs in
`ci.yml` and `broadcast-windows.yml` immediately after
`attach-release-assets`, gated on that action's new `attached` output. The
attach gate ("does this component's tag point at THIS commit") is the only
thing that knows whether a release just received bytes; a manifest written
on any other condition — the release-please tag existing, the release
commit landing — would for some minutes point a download button at a 404,
and permanently so on a release whose build failed. The manifest describes
the same asset directory the attach action uploaded, so the checksums are
those of the bytes on the release page, not a recomputation.

**D3 — The page reads the manifest at load time; the site stays
build-free.** `site/site.js` fetches the two manifests and fills the cards
in. The alternative — rendering the versions into the HTML in the Pages
workflow — would need a cross-workflow trigger from two attach jobs into
`pages.yml` (a push to `main` fires Pages minutes before the assets exist,
see D2), a generation step in a site that deliberately has none, and a
deploy per release that today happens only when `site/` changes. Reading
the manifest client-side costs one small request per card, made to the same
host the page already trusts.

**D4 — The markup works without the manifest.** Each card ships with a
button that links to the releases page filtered to that component, a
"Latest release" line and no checksum. The script *upgrades* the card — the
button becomes a direct download, the line becomes `vX.Y.Z · date · size`
linking to the release notes, and the checksum appears — only when the
manifest fetched, parsed and passed the same shape check the writer
enforces. A blocked fetch, a stale CDN copy, an unpublished manifest, or a
manifest with an unexpected field type all leave the fallback in place. The
script trusts nothing from the manifest as HTML, and only follows URLs
under `https://github.com/Tuhis/gawk/releases/`.

**D5 — Manifest shape (`schema: 1`).**

```json
{
  "schema": 1,
  "component": "gawk-broadcast",
  "version": "1.13.0",
  "tag": "gawk-broadcast/v1.13.0",
  "published_at": "2026-09-10T19:54:22Z",
  "release_url": "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast/v1.13.0",
  "asset": { "name": "gawk-broadcast-linux-amd64.tar.gz", "size": 27412345, "sha256": "…", "url": "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast/v1.13.0/gawk-broadcast-linux-amd64.tar.gz" },
  "assets": { "gawk-broadcast-linux-amd64.tar.gz": { "size": …, "sha256": "…", "url": "…" }, "SHA256SUMS": { … } }
}
```

`asset` is the file the download button points at (the tarball on Linux,
the EXE on Windows) and restates its `assets` entry so a consumer that only
wants the button reads one object. `assets` lists everything attached,
checksum file included, so R45 phase 2 can fetch and verify by name.
`tools/releases/manifest.py validate` is the shape's definition; the writer
runs it before pushing and the tests pin it. Extra keys are tolerated so a
later schema can add without breaking an older reader; a changed meaning
bumps `schema`. `tag` may be the legacy `component-vX.Y.Z` spelling — a
backfill of a release cut before the tag-separator change runs the writer
on exactly that tag, and the download URL works with either.

**D6 — Byte-stable output, no empty commits.** The writer sorts keys and
ends with a newline; a re-run for the same release (the attach action's own
recovery path) produces identical bytes and the branch writer makes no
commit. Concurrent pushes to `badges` — the coverage writer runs from four
jobs on the same release commit — are handled by the same
fetch-rewrite-retry loop `publish-coverage-badges` uses; this action's whole
change is one file, so "rewrite" is a copy.

**D7 — The first two manifests are seeded from the shipped assets, not a
rebuild.** The section is live from the moment this lands, before any new
release, by running the same `manifest.py build` locally against the
downloaded `gawk-broadcast/v1.13.0` and `gawk-broadcast-windows/v1.3.0`
assets and pushing the result to `badges`. A backfill dispatch would have
rebuilt both binaries and re-uploaded them, and a rebuilt binary is not
guaranteed byte-identical, so its checksum would no longer be the one the
existing release page states. Seeding is a one-off; from the next
broadcaster release on, D2 does it.

### Rejected

- **`releases/latest/download/<asset>`** — the stable GitHub URL for the
  repo-wide latest release. Wrong for per-component tags (D1).
- **Rendering the version from `.release-please-manifest.json` at Pages
  build time.** Zero JS and zero API, but the manifest changes on the
  release commit and the assets exist only minutes later, so the page would
  link to a 404 in between; closing that needs the cross-workflow trigger
  D3 declined.
- **Fetching the release-please manifest client-side** from
  raw.githubusercontent.com. Same timing hole, and it carries no checksum,
  size or date.
- **A hero call-to-action** — the hero stays on the browser-first pitch;
  Download is a nav link and a section (owner decision 2026-09-14).

## 3. Where it plugs in

```
.github/actions/attach-release-assets/    +outputs: attached, tag, published_at
.github/actions/publish-release-manifest/ NEW: build + push releases/<component>/latest.json
.github/workflows/ci.yml                  attach-broadcast-release: manifest step; release-tool job
.github/workflows/broadcast-windows.yml   attach-release: manifest step
.github/actions/publish-coverage-badges/  badges-branch README now describes releases/
tools/releases/manifest.py                the writer + validator; tests alongside
site/index.html                           #download section, nav link
site/site.css                             .dl-* rules
site/site.js                              manifest fetch + card fill
```

The `badges` branch after this holds, beside the coverage files,
`releases/gawk-broadcast/latest.json` and
`releases/gawk-broadcast-windows/latest.json`.

## 4. Chunks and acceptance criteria

| Chunk | Deliverable | Accepted when |
|---|---|---|
| **DL1** | `tools/releases/manifest.py` + tests; `publish-release-manifest` action; `attached`/`tag`/`published_at` outputs on `attach-release-assets`; both attach jobs run the manifest step | `python3 -m unittest discover -s tools/releases` passes; a release commit that attaches `gawk-broadcast` assets leaves `releases/gawk-broadcast/latest.json` on `badges` whose `asset.sha256` equals the `SHA256SUMS` line on the release; a push that attaches nothing writes nothing; a re-run for the same release makes no commit |
| **DL2** | The two current releases seeded (D7) | both manifests exist on `badges`, validate, and their checksums match the release pages' `SHA256SUMS` |
| **DL3** | The `#download` section, nav link, styles, script | with the manifests reachable each card shows `vX.Y.Z · date · size`, a button whose `href` is the asset URL and the matching sha256; with `site.js` disabled (or the fetch blocked) each card still shows a button to the releases page and no checksum; the page passes at 400 px width without horizontal scroll |
| **DL4** | This document, the ROADMAP row and entry, the docs index, the R45 entry pointing here for its manifest, `release-tool` CI job | present; `docs/README.md` lists this doc under Product surface; a change to `tools/releases/` runs the job |
| **DL5** | Footer links from the web UI to the site (§6) | the landing footer reads About · Get the app · Terms of use · GitHub; About opens the site root and Get the app opens `#download`, both in a new tab with `rel="noopener noreferrer"`; `LandingPage.test.tsx` pins all of it |

## 5. Security considerations

- The site executes nothing from the manifest. Values are assigned to
  `textContent` and to `href` after a prefix check against
  `https://github.com/Tuhis/gawk/releases/`; a manifest an attacker could
  write (which would already mean write access to the repository) can at
  worst point the button at a different file on the project's own releases
  page.
- `GITHUB_TOKEN` with `contents: write` is what the attach jobs already
  hold; the manifest push adds no permission and no secret.
- The manifest carries nothing about visitors; the fetch is a plain `GET`
  of a static file with no identifying parameter — the property R45 requires
  of the desktop apps' request, kept from day one.

## 6. Links from the web UI (DL5, added 2026-09-14)

The SPA's landing footer is the one place a user of the deployed app meets
the project, and until now it offered only the terms and the source
repository. It gains two quiet links, same weight as the existing pair:

- **About** → the site root (`SITE_URL` in `config.ts`), for what gawk is
  and how it works.
- **Get the app** → `SITE_URL#download`, straight to the section this
  document describes. Separate from About on purpose: the native
  broadcasters are the one thing this UI may send someone to fetch, and a
  link that lands on the hero and asks them to scroll is a link they will
  not follow.

Both are constants, not runtime config, for the reason `SOURCE_URL` is:
they describe the project every deployment is built from, not the
deployment. A fork that wants its own site edits them in the fork. Both
open in a new tab with `rel="noopener noreferrer"` so the join card is
never navigated away from. Order: About · Get the app · Terms of use ·
GitHub — project first, obligations and source after.

Not added: links from the viewer or broadcaster screens. Those surfaces are
deliberately chrome-free (docs/10, docs/29's viewer "⋮" menu is the one
concession) and someone already streaming has no need of the download.
