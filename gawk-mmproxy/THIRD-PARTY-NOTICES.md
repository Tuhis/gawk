# Third-party notices — `gawk-mmproxy`

The real-address sidecar for murmur (`gawk-mmproxy`). It depends on
the Go standard library and `golang.org/x/sys` only, so this list is
the whole of what its image carries besides the Go runtime.

This file is generated. Do not edit it by hand — run
`python3 tools/licenses/gen-notices.py` and commit the result. See that
script for what counts as a dependency here and why.

**Scope:** `go list -deps ./...` in `gawk-mmproxy/`.

## Summary — 1 package

| License (as declared) | Packages |
|---|---:|
| `BSD-3-Clause` | 1 |

## Packages

| Package | Version | License | Copyright |
|---|---|---|---|
| `golang.org/x/sys` | v0.50.0 | `BSD-3-Clause` | Copyright 2009 The Go Authors |

## License texts

Each distinct license text below appears once. Texts that differ only in
their copyright line are shown once, with every holder listed in the table
above — that line is the part the license requires be retained, and the
boilerplate around it is identical by construction.

### 1. BSD-3-Clause — 1 package

Applies to: `golang.org/x/sys`

```
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

