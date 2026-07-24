# gawk-app Helm chart

Static-file SPA (nginx) for the gawk viewer/broadcaster surfaces, served behind
an internal Ingress. The relay is a separate chart (`gawk-server`).

Runtime configuration is injected without rebuilding the image: the chart
renders a `/config.js` (and, for R23, a `/terms.html`) from a ConfigMap that is
mounted over the shipped defaults and read by the app at load. See
`values.yaml` `config.*` and `gawk-app/src/config.ts`.

## Terms of use (R23, docs/29)

The app ships a default **Terms of Use** text, reachable at `#/terms` from every
surface. A broadcaster acknowledges the terms once before their first broadcast
(viewers are never gated). You can present the terms as your own deployment's
terms via `config.*` values — no image rebuild.

> ⚠️ **The default terms text is a protective template, not legal advice.** It
> was written to a self-hoster's priorities (no warranty, broadcaster is
> responsible for their content, content must be lawful in Finland and in the
> broadcaster's own country, broad operator moderation rights). Before you
> expose a deployment beyond a closed circle of people you know, have a Finnish
> lawyer review it — especially the liability, consumer-law, and personal-data
> clauses, which interact with mandatory EU/Finnish provisions that no contract
> can waive. Shipping the template is fine; treating it as vetted legal cover is
> not.

### Depth 1 — identity only (common case)

The default text renders with your name + contact substituted in:

```yaml
config:
  operatorName: "Juho's homelab"
  operatorContact: "gawk@example.com"
  # Bump on any meaningful edit to re-prompt broadcasters for acknowledgment.
  termsVersion: "2026-07-24"
```

### Depth 2 — replace the whole text

Provide your own HTML body and point the app at it:

```yaml
config:
  termsUrl: "/terms.html"
  termsVersion: "2026-07-24"
  termsHtml: |
    <h1>Terms of Use</h1>
    <p>Your own terms here…</p>
```

The body is fetched on the terms page and **sanitized** (allowlist: headings,
paragraphs, lists, emphasis, tables, and http/https/mailto links survive;
scripts, styles, iframes, event handlers, and other schemes are stripped)
before it is rendered. If the fetch fails or is empty, the app falls back to the
bundled default, so the page is never blank.

`termsVersion` is an explicit string, not a content hash: bump it to re-prompt
broadcasters, leave it unchanged for typo fixes that shouldn't re-prompt.
