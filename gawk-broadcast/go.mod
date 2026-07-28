module github.com/Tuhis/gawk/gawk-broadcast

go 1.26.0

require (
	gioui.org v0.10.1
	github.com/Tuhis/gawk/gawk-server v0.0.0
	github.com/godbus/dbus/v5 v5.2.2
	github.com/quic-go/quic-go v0.61.0
	github.com/quic-go/webtransport-go v0.12.0
)

require (
	gioui.org/shader v1.0.8 // indirect
	github.com/dunglas/httpsfv v1.1.0 // indirect
	github.com/go-text/typesetting v0.3.4 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp/shiny v0.0.0-20250408133849-7e4ce0ab07d0 // indirect
	golang.org/x/image v0.26.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)

// The relay module is not published as a versioned dependency: the wire
// package is consumed from this repo's tree so any wire change breaks this
// module's build or its golden-vector tests immediately (R14 Decision 11).
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server
