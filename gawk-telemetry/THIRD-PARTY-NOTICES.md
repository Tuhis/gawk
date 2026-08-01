# Third-party notices — `gawk-telemetry`

The optional diagnostics service, listed as the *deployed* image builds
it: `-tags duckdb`. A default (tag-free) build links no third-party Go
code at all. The bundled dashboard UI is covered by
`ui/THIRD-PARTY-NOTICES.md`.

This file is generated. Do not edit it by hand — run
`python3 tools/licenses/gen-notices.py` and commit the result. See that
script for what counts as a dependency here and why.

**Scope:** `go list -deps -tags duckdb ./...` in `gawk-telemetry/`.

## Summary — 5 packages

| License (as declared) | Packages |
|---|---:|
| `MIT` | 4 |
| `BSD-3-Clause` | 1 |

## Packages

| Package | Version | License | Copyright |
|---|---|---|---|
| `github.com/duckdb/duckdb-go-bindings/linux-amd64` | v0.1.21 | `MIT` | Copyright 2018-2025 Stichting DuckDB Foundation |
| `github.com/go-viper/mapstructure/v2` | v2.4.0 | `MIT` | Copyright (c) 2013 Mitchell Hashimoto |
| `github.com/google/uuid` | v1.6.0 | `BSD-3-Clause` | Copyright (c) 2009,2014 Google Inc. All rights reserved |
| `github.com/marcboeker/go-duckdb/mapping` | v0.0.21 | `MIT` | Copyright 2019 Marc Boeker |
| `github.com/marcboeker/go-duckdb/v2` | v2.4.3 | `MIT` | Copyright 2019 Marc Boeker |

## License texts

Each distinct license text below appears once. Texts that differ only in
their copyright line are shown once, with every holder listed in the table
above — that line is the part the license requires be retained, and the
boilerplate around it is identical by construction.

### 1. BSD-3-Clause — 1 package

Applies to: `github.com/google/uuid`

```
Copyright (c) 2009,2014 Google Inc. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
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

### 2. MIT — 3 packages

Applies to: `github.com/duckdb/duckdb-go-bindings/linux-amd64`, `github.com/marcboeker/go-duckdb/mapping`, `github.com/marcboeker/go-duckdb/v2`

```
Copyright 2019 Marc Boeker

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```

### 3. MIT — 1 package

Applies to: `github.com/go-viper/mapstructure/v2`

```
The MIT License (MIT)

Copyright (c) 2013 Mitchell Hashimoto

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```

