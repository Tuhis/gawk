// Package murmurice is the minimum of ZeroC Ice gawk-mumble-bridge needs
// from murmur (R85, docs/89 D7): Ice protocol 1.0 over TCP, encoding 1.1.
//
// The client sends six operations and nothing else: Meta.getBootedServers,
// Meta.getVersion, Server.getUsers, Server.getChannels, Server.addCallback
// and Server.removeCallback, twoway, multiplexed on one connection, each
// with a deadline. The endpoint is the other direction: murmur dials it to
// deliver the seven ServerCallback events, oneway (F12).
//
// Every reply and event must decode to exactly its end, so a murmur whose
// User layout changes fails with a *ProtocolError instead of producing a
// wrong roster. No classes, batch requests, compression, TLS endpoints or
// locators.
//
// What the package keeps from murmur is deliberately less than murmur
// sends (D10): every User carries the user's IP address, comment, identity
// and plugin context, and the decoder skips those bytes without storing
// them. Nothing here logs a payload or the secret.
//
// Modelled on github.com/Tuhis/mumisija/ice (protocol.go, conn.go,
// encoding.go), reimplemented as the minimum (docs/89 §2).
package murmurice
