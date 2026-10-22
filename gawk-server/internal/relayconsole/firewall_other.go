//go:build !windows

package relayconsole

import "context"

// FirewallSupported reports that this build cannot add the rule: off
// Windows the console shows no firewall line.
const FirewallSupported = false

// AllowFirewallRule is Windows-only.
func AllowFirewallRule(context.Context, []string) error { return ErrFirewallUnsupported }

// FirewallRulePresent is Windows-only.
func FirewallRulePresent(context.Context) (bool, error) { return false, ErrFirewallUnsupported }
