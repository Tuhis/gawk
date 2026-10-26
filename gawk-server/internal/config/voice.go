package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// VoiceConfigured reports whether R79 room voice is on (docs/80 §11.4):
// rooms on and all four bridge knobs set. ParseFlags refuses a partial set,
// so for a parsed config "any is set" and "all are set" agree; checking all
// four keeps a hand-built test config honest too.
func (c Config) VoiceConfigured() bool {
	return c.Rooms && c.VoiceBridgeAddr != "" && c.VoiceBridgePublicURL != "" &&
		c.VoiceBridgePSK != "" && c.VoiceTicketSecret != ""
}

// parseVoice validates the voice knobs. With none of the four set, voice is
// off and only the caps are checked (they still parse, so a bad value fails
// at deploy time rather than the day voice is turned on). The returned
// Config carries only the voice fields.
func parseVoice(roomsOn bool, addr, publicURL, psk, secret, serverName, caFile, maxRooms, maxUsers string) (Config, error) {
	addr, publicURL = strings.TrimSpace(addr), strings.TrimSpace(publicURL)
	serverName, caFile = strings.TrimSpace(serverName), strings.TrimSpace(caFile)
	var c Config
	n, err := strconv.Atoi(maxRooms)
	if err != nil || n < 1 {
		return c, fmt.Errorf("invalid voice-max-rooms %q: want a positive integer", maxRooms)
	}
	c.VoiceMaxRooms = n
	if n, err = strconv.Atoi(maxUsers); err != nil || n < 1 {
		return c, fmt.Errorf("invalid voice-max-users-per-room %q: want a positive integer", maxUsers)
	}
	c.VoiceMaxUsersPerRoom = n

	set := 0
	for _, v := range []string{addr, publicURL, psk, secret} {
		if v != "" {
			set++
		}
	}
	if set == 0 {
		if serverName != "" || caFile != "" {
			return c, fmt.Errorf("voice-bridge-server-name and voice-bridge-ca-file need voice configured (-voice-bridge-addr, -voice-bridge-public-url, -voice-bridge-psk, -voice-ticket-secret)")
		}
		return c, nil
	}
	if set != 4 {
		return c, fmt.Errorf("room voice needs all of -voice-bridge-addr, -voice-bridge-public-url, -voice-bridge-psk and -voice-ticket-secret (%d of 4 set)", set)
	}
	if !roomsOn {
		return c, fmt.Errorf("room voice (-voice-bridge-addr …) requires -rooms")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return c, fmt.Errorf("invalid voice-bridge-addr %q: want host:port", addr)
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(publicURL) > wire.MaxVoiceURLLen {
		return c, fmt.Errorf("invalid voice-bridge-public-url %q: want an absolute https URL of at most %d bytes", publicURL, wire.MaxVoiceURLLen)
	}
	if len(secret) < voice.MinSecretLen {
		return c, fmt.Errorf("voice-ticket-secret is %d bytes, want at least %d", len(secret), voice.MinSecretLen)
	}
	if serverName == "" {
		serverName = host
	}
	c.VoiceBridgeAddr, c.VoiceBridgePublicURL, c.VoiceBridgePSK, c.VoiceTicketSecret = addr, publicURL, psk, secret
	c.VoiceBridgeServerName, c.VoiceBridgeCAFile = serverName, caFile
	return c, nil
}
