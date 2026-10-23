package popclient

// budget.go: the host's budgets (docs/76 §6.8, §7.1). -uplink-budget and
// -downlink-budget are the configured ones; the console can lower either
// at runtime, in memory only, and LowerBudgets is how that reaches the
// legs' admission (uplink.go, downlink.go) and the stats record.

import "github.com/Tuhis/gawk/gawk-server/internal/config"

// LowerBudgets sets the console's lowered uplink and downlink budgets. A
// zero field is the configured value; a field above the configured one
// is ignored, so this never raises a budget.
func (c *Client) LowerBudgets(up, down config.Budget) {
	c.mu.Lock()
	c.lowerUp, c.lowerDown = up, down
	c.mu.Unlock()
}

// hostUplink is the host's uplink budget in force: -uplink-budget, each
// field lowered by the console.
func (c *Client) hostUplink() config.Budget {
	c.mu.Lock()
	l := c.lowerUp
	c.mu.Unlock()
	return lowered(c.opts.Relay.UplinkBudget, l)
}

// hostDownlink is the host's downlink budget in force.
func (c *Client) hostDownlink() config.Budget {
	c.mu.Lock()
	l := c.lowerDown
	c.mu.Unlock()
	return lowered(c.opts.Relay.DownlinkBudget, l)
}

// lowered is configured with each field replaced by the lower one, where
// set; zero is unlimited on the configured side.
func lowered(configured, lower config.Budget) config.Budget {
	return config.Budget{
		Mbps:    lowerOf(configured.Mbps, lower.Mbps),
		Streams: lowerOf(configured.Streams, lower.Streams),
	}
}
