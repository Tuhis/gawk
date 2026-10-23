package events

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// R74 (docs/76 §8): broadcast events say which PoP relay a broadcast is
// published through, by name — and never where it is.

// popBroadcastTypes are the hub-published broadcast events that carry pop.
var popBroadcastTypes = []string{
	TypeBroadcastStarted, TypeBroadcastEnded, TypeBroadcastPublisherAway,
	TypeBroadcastPublisherBack, TypeBroadcastViewers,
}

func TestBroadcastEventsNameTheirPop(t *testing.T) {
	c := load(t)
	for _, tp := range popBroadcastTypes {
		var v struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(c.vectors[tp], &v); err != nil {
			t.Fatal(err)
		}
		if v.Data["pop"] != fixturePop {
			t.Errorf("vector %s: data.pop = %v, want %q", tp, v.Data["pop"], fixturePop)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		}
		if err := json.Unmarshal(c.schemas[SchemaFile(tp)], &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["pop"]; !ok {
			t.Errorf("schema of %s does not declare pop", tp)
		}
		for _, r := range schema.Required {
			if r == "pop" {
				t.Errorf("schema of %s requires pop: a home broadcast has none", tp)
			}
		}
	}
}

// TestPopIsANameNeverAnAddress: the pop property refuses an address, and no
// event schema declares an address-shaped property at all (docs/76 §8: no
// event, on the bus or in a webhook, carries a publisher IP).
func TestPopIsANameNeverAnAddress(t *testing.T) {
	c := load(t)
	for _, addr := range []string{"203.0.113.7", "2001:db8::1", "venue.example.com:4433"} {
		m := c.clone()
		m.vectors[TypeBroadcastStarted] = bytes.Replace(m.vectors[TypeBroadcastStarted],
			[]byte(`"pop": "assembly-2026"`), []byte(`"pop": "`+addr+`"`), 1)
		mustFail(t, m.checkVectors(), "data of fi.ioio.gawk.broadcast.started does not validate")
	}

	addrName := regexp.MustCompile(`(?i)(^|[a-z])(ip|ips|addr|address|addresses|remote|lanaddr|publicaddrs?)$`)
	for name, raw := range c.schemas {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		walkSchema(doc, "", func(path string, node map[string]any) {
			props, _ := node["properties"].(map[string]any)
			for p := range props {
				if addrName.MatchString(p) || strings.Contains(strings.ToLower(p), "address") {
					t.Errorf("schema/%s%s declares %q, an address-shaped property", name, path, p)
				}
			}
		})
	}
}
