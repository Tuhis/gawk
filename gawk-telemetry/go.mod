module github.com/Tuhis/gawk/gawk-telemetry

go 1.26.0

// The relay module is not published as a versioned dependency: the wire
// package (and its telemetry session tokens) are consumed from this repo's
// tree so any change to the token format breaks this module's build or its
// tests immediately — the same rule gawk-broadcast follows (R14 Decision 11).
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server

require github.com/Tuhis/gawk/gawk-server v0.0.0-00010101000000-000000000000
