# badges

Generated on pushes to `main`. Nothing here is source — do not edit it
by hand, and do not merge this branch anywhere.

- Top level: `.github/actions/publish-coverage-badges`. `data.json`
  holds the coverage counts; every other file is a
  [shields.io endpoint](https://shields.io/badges/endpoint-badge)
  response consumed by the README badges.
- `releases/<component>/latest.json`:
  `.github/actions/publish-release-manifest`, written when a native
  broadcaster release gets its binaries attached. The project site's
  Download section reads it (docs/46).
