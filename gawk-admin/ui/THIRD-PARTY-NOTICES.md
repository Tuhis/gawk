# Third-party notices — `ui`

The moderation portal SPA, embedded into the `gawk-admin` binary.
Build-time-only packages are excluded, as for `gawk-app`. The OIDC
public-client flow is hand-rolled against WebCrypto (docs/42 §4.8),
so no OIDC library appears here.

This file is generated. Do not edit it by hand — run
`python3 tools/licenses/gen-notices.py` and commit the result. See that
script for what counts as a dependency here and why.

**Scope:** `package-lock.json` entries without `dev: true`.

## Summary — 3 packages

| License (as declared) | Packages |
|---|---:|
| `MIT` | 3 |

## Packages

| Package | Version | License | Copyright |
|---|---|---|---|
| `react` | 19.2.8 | `MIT` | Copyright (c) Meta Platforms, Inc. and affiliates |
| `react-dom` | 19.2.8 | `MIT` | Copyright (c) Meta Platforms, Inc. and affiliates |
| `scheduler` | 0.27.0 | `MIT` | Copyright (c) Meta Platforms, Inc. and affiliates |

## License texts

Each distinct license text below appears once. Texts that differ only in
their copyright line are shown once, with every holder listed in the table
above — that line is the part the license requires be retained, and the
boilerplate around it is identical by construction.

### 1. MIT — 3 packages

Applies to: `react`, `react-dom`, `scheduler`

```
MIT License

Copyright (c) Meta Platforms, Inc. and affiliates.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

