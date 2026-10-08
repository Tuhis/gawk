module github.com/Tuhis/gawk/gawk-broadcast

go 1.26.0

require (
	github.com/Tuhis/gawk/gawk-server v0.0.0
	github.com/quic-go/quic-go v0.62.0
	github.com/quic-go/webtransport-go v0.13.0
)

require (
	github.com/dunglas/httpsfv v1.1.1 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

// The relay module is not published as a versioned dependency: the wire
// package is consumed from this repo's tree so any wire change breaks this
// module's build or its golden-vector tests immediately (R14 Decision 11).
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server
