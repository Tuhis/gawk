module github.com/Tuhis/gawk/gawk-mumble-bridge

go 1.26.0

// The voice wire (VoiceHello/Frame/State/Control, SessionClosing, the room
// record framing) and the relay ↔ bridge contract (the ticket, the channel
// key, the link messages) live in gawk-server's public packages, so the
// bridge compiles the relay's own definitions instead of mirroring them
// (docs/80 §11.2). Same local-replace shape gawk-sampler uses.
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server

require (
	github.com/Tuhis/gawk/gawk-server v0.0.0-00010101000000-000000000000
	github.com/prometheus/client_golang v1.25.0
	github.com/quic-go/quic-go v0.62.0
	github.com/quic-go/webtransport-go v0.13.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dunglas/httpsfv v1.1.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/quic-go/qpack v0.6.0 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)
