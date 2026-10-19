// Command gawk-uplinklab measures the broadcaster → relay leg (docs/74):
// a receiver built on the relay's own QUIC stack and configuration, and a
// deterministic UDP impairment proxy to put in front of it.
//
//	gawk-uplinklab recv  -addr 127.0.0.1:4500 -out recv.csv
//	gawk-uplinklab proxy -listen 127.0.0.1:4600 -upstream 127.0.0.1:4500 -rtt 10ms -loss 0.01
//	gawk-uplinklab sub   -url https://127.0.0.1:4433 -id-file id.txt -out sub.csv
//
// The sender is the Rust tool in gawk-broadcast-desktop/tools/uplinklab,
// which dials with the engine's quinn configuration. Every timestamp is the
// host's wall clock in nanoseconds, so sender and receiver logs on one host
// share a clock.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "recv":
		err = runRecv(os.Args[2:])
	case "proxy":
		err = runProxy(os.Args[2:])
	case "sub":
		err = runSub(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gawk-uplinklab:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gawk-uplinklab recv|proxy|sub [flags]")
	os.Exit(2)
}
