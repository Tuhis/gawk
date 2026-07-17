// Package wirecheck holds no code — only tests that bind this module to the
// relay's wire format from the consumer side (R14 Decision 11, docs/19).
//
// The native broadcaster is a second implementation of the publisher protocol,
// and the standing risk is that it silently rots when the format moves. Two
// mechanisms prevent that, and this package is the second one:
//
//  1. Compile-time coupling: the engine imports github.com/Tuhis/gawk/gawk-server/wire
//     through a local replace directive, so a changed signature or a deleted
//     constant fails this module's build.
//  2. Byte-level coupling (here): the golden vectors from wire_test.go are
//     re-asserted from this side, so a change that keeps the API but moves the
//     bytes — a reordered header field, a resized constant — fails this
//     module's tests too, not just the relay's.
//
// If a test here fails, the wire format changed. That is a protocol decision
// affecting the relay, both broadcasters and every viewer; update all of them
// deliberately rather than editing the expectation.
package wirecheck
