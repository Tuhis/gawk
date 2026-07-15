module github.com/Tuhis/gawk/gawk-broadcast

go 1.25.0

require (
	github.com/Tuhis/gawk/gawk-server v0.0.0
	github.com/quic-go/quic-go v0.60.0
	github.com/quic-go/webtransport-go v0.11.1
)

require (
	github.com/dunglas/httpsfv v1.1.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)

// The relay module is not published as a versioned dependency: the wire
// package is consumed from this repo's tree so any wire change breaks this
// module's build or its golden-vector tests immediately (R14 Decision 11).
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server
