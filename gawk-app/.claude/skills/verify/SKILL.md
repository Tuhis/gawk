---
name: verify
description: Drive the built gawk-app in a real browser (headless system Chrome via playwright-core) to verify frontend changes at the UI surface.
---

# Verifying gawk-app changes in a real browser

Build + serve (no relay needed for UI-level checks — the viewer settles into
its error card after ~2 s, but the overlay, context menu, controls, and the
whole worker boot/init message flow all run regardless):

```bash
cd gawk-app && npm run build && npm run preview -- --port 4173 &
```

Drive with playwright-core + system Chrome (no browser download):

```bash
mkdir -p /tmp/gawk-verify && cd /tmp/gawk-verify && npm i playwright-core
```

```js
import { chromium } from 'playwright-core';
const browser = await chromium.launch({
  executablePath: '/usr/bin/google-chrome', headless: true });
```

Recipes that worked:

- Viewer page: `http://localhost:4173/#/view/AB2CD3` (any 6-char code).
- **Right-click menu**: the connecting/error card intercepts locator clicks
  on the canvas — use raw `page.mouse.click(200, 200, { button: 'right' })`
  instead; the contextmenu bubbles to the viewer root.
- **Stats overlay rows**: `page.locator('dt', { hasText: 'Row label' })`
  then `locator('xpath=following-sibling::dd').textContent()`.
- **Element fullscreen works in headless Chrome** (real `fullscreenElement`,
  `fullscreenchange` fires) when triggered from a Playwright click.
- **Simulate an iPhone-gated device** (R16): 
  `context.addInitScript(() => { delete Element.prototype.requestFullscreen; delete Document.prototype.exitFullscreen; })`
  — the R16 gate flips, and Chrome's worker lacking `VideoTrackGenerator`
  exercises the probe-failed → pseudo-fullscreen tier for real.
- **Assert worker message traffic** via an init script wrapping
  `Worker.prototype.postMessage` and recording `{type, keys}` per message.
- CSS-module classes are hashed — assert with a regex like
  `/pseudoFullscreen/` against `className`, never exact match.
