## How to build with gawk

gawk is the UI of a self-hosted, low-latency game-streaming app: a broadcaster
shares their screen and viewers watch in the browser. Most of the surface is
**chrome floating over live video**, which is what the visual language is for.

### Dark-only. There is no light theme.

`:root` sets `color-scheme: dark` and the stylesheet paints `html, body, #root`
with `--bg` (`#0a0b0d`) and `--text` (`#e8eaed`). **Never write a light
background or a dark-on-light pairing** — `Button variant="primary"` is a
*near-white fill with near-black text*, and `variant="ghost"` is muted gray.
Both look broken on white.

**There is no provider and no wrapper component.** Theming is plain CSS custom
properties, and `styles.css` carries them, so components are styled the moment
they render. Just render them:

```jsx
<Button variant="primary">Go live</Button>
```

The only setup rule: if you build a container that fills the screen, give it
`background: var(--bg); color: var(--text)` so it doesn't inherit a host page's
white.

### The styling idiom: CSS custom properties. No utility classes.

There is **no Tailwind, no class vocabulary, no style props**. Components own
their look through CSS Modules you cannot see or extend. For your own layout
glue, write plain CSS or inline styles and reach for these tokens — this is the
complete set:

| Group | Tokens |
|---|---|
| Canvas & surfaces | `--bg` `--surface-1` `--surface-2` `--border` `--border-soft` |
| Glass (chrome over video) | `--glass` `--glass-border` `--glass-blur` |
| Text | `--text` `--muted` `--faint` |
| Accent (used sparingly) | `--accent` `--accent-hover` `--accent-soft` |
| Status | `--live` `--danger` `--warning` `--ok` |
| Type scale | `--fs-hero` `--fs-lg` `--fs-md` `--fs-sm` `--fs-xs` |
| Radius / shadow | `--r-sm` `--r-md` `--r-lg` `--shadow` |
| Motion | `--dur-fast` `--dur` `--ease` |
| Legacy alias | `--panel` (= `--surface-1`) |

Use them as `var(--surface-2)`. **Never hardcode a hex color, radius or
duration** — every gawk surface is built from this list, and a literal breaks the
one-product read. The type stack is the system stack
(`system-ui, -apple-system, Segoe UI, Roboto, sans-serif`); no webfont ships or
is expected.

Two house rules the design carries deliberately:

- **Monochrome first.** `--accent` is for focus rings and the active/live state
  only. Status colors mean status — `--live`/`--danger` red is a broadcast that
  is live or an action that ends one, not decoration.
- **Chrome floats on glass.** Anything layered over the stream goes in
  `GlassPanel` (or a component built on it: `StatsPanel`, `StatsOverlay`,
  `BroadcasterStatsOverlay`, `ViewerSettingsPanel`, `ServerPickerPanel`). It is
  a translucent, blurred, shadowed surface — it only reads correctly over
  content, so don't put it on a flat background.

### Where the truth lives

- **`styles.css`** and the `_ds_bundle.css` it imports — every token
  definition and every component rule live there. Read it before styling.
- **`components/<group>/<Name>/<Name>.prompt.md`** — per-component usage.
- **`components/<group>/<Name>/<Name>.d.ts`** — the prop contract. Honour it;
  several components are purely presentational and expect **pre-formatted
  strings** (`StatsPanel`/`StatsGrid` rows take `"7.8 Mbps"`, and `"—"` for
  unavailable — they never format or compute anything themselves).

### An idiomatic composition

```jsx
<div style={{ background: 'var(--bg)', color: 'var(--text)', padding: '24px' }}>
  <GlassPanel style={{ padding: '20px 24px', maxWidth: 360 }}>
    <p style={{ margin: '0 0 6px', fontSize: 'var(--fs-lg)', fontWeight: 600 }}>
      Sharing tips
    </p>
    <p style={{ margin: 0, color: 'var(--muted)', fontSize: 'var(--fs-sm)' }}>
      Pick <strong>Entire screen</strong> so fullscreen games keep streaming.
    </p>
    <div style={{ display: 'flex', gap: 12, marginTop: 16 }}>
      <Button variant="primary">Share my screen</Button>
      <Button variant="ghost">Cancel</Button>
    </div>
  </GlassPanel>
</div>
```

Icons (`GearIcon`, `CloseIcon`, `ServerIcon`, …) draw with `currentColor` and are
sized `1em`, so they take the color and size of their context — set `font-size`
to scale them. `IconButton` requires a `label`; it is the accessible name.
