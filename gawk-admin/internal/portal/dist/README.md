# Build output

`ui/`'s Vite build writes here (`cd ui && npm run build`), and `portal.go`
embeds the whole directory. Everything except this file is generated and
git-ignored.

This file exists so the directory is never empty: `//go:embed dist` fails to
compile against a directory with no matching files, which would make a fresh
clone unbuildable until someone had run npm. With it, `go build ./...` always
works — it just serves a "UI not built" page until you run the build.
