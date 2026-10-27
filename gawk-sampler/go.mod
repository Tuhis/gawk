module github.com/Tuhis/gawk/gawk-sampler

go 1.26.0

// The screening contract (target list, content-flag body, sample batch, the
// D7 window rule, Shard) and the wire parsers live in gawk-server's public
// packages, so the sampler compiles the relay's own definitions instead of
// mirroring them (docs/71 D3). Same local-replace shape gawk-admin and
// gawk-telemetry use.
replace github.com/Tuhis/gawk/gawk-server => ../gawk-server

require (
	github.com/Tuhis/gawk/gawk-server v0.0.0-00010101000000-000000000000
	github.com/prometheus/client_golang v1.25.0
	github.com/yalue/onnxruntime_go v1.36.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
