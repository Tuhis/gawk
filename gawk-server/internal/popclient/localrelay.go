package popclient

import (
	"os"
	"unicode/utf8"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// LocalRelayInfo is the PoP's 0x18 for one session (transport's
// LocalRelayProvider, installed by cmd/gawk-relay with SetLocalRelay):
// linked and home's app origin while the link is up, the name alone when
// standalone, revoked or offline (docs/76 item 45). The name is home's
// display name for this PoP when linked, else -server-name, else the
// hostname. The wall fields are W3-wall's.
func (c *Client) LocalRelayInfo(publishID string) wire.LocalRelayInfo {
	st := c.State()
	var info wire.LocalRelayInfo
	if st.Header == HeaderLinked && st.Config != nil {
		info.Linked = true
		info.HomeOrigin = st.Config.HomeOrigin
		info.Name = st.Config.DisplayName
	}
	if info.Name == "" {
		info.Name = c.opts.Cfg.ServerName
	}
	if info.Name == "" {
		hostname := c.opts.Hostname
		if hostname == nil {
			hostname = os.Hostname
		}
		if h, err := hostname(); err == nil {
			info.Name = h
		}
	}
	info.Name = truncateUTF8(info.Name, wire.MaxRelayIdentityNameLen)
	c.wall.fill(&info, publishID)
	// A home origin the wire refuses (a misconfigured home) must not cost
	// the whole notice: send it without the origin.
	if _, err := wire.AppendLocalRelayInfo(nil, info); err != nil {
		info.HomeOrigin = ""
	}
	return info
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
