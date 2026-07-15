module github.com/Tuhis/gawk/gawk-broadcast

go 1.25.0

require github.com/Tuhis/gawk/gawk-server v0.0.0

// The relay module is not published as a versioned dependency: the wire
// package is consumed from this repo's tree so any wire change breaks this
// module's build or its golden-vector tests immediately (R14 Decision 11).
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server
