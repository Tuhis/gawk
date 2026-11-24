package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

// R87 (docs/91 SG3): the deep-buffer stripe fields arrive typed, round-trip
// through the stored JSON, and are catalogued.
func TestStripedDVRFieldsRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		want     []float64 // nil: connectionBytesReceived absent
		dropped  int
		widthSet bool
	}{
		{"striped ×2", `{"dvrStripeWidth":2,"connectionBytesReceived":[1200000,1180000]}`, []float64{1.2e6, 1.18e6}, 0, true},
		{"unstriped", `{"dvrStripeWidth":1,"connectionBytesReceived":[42]}`, []float64{42}, 0, true},
		{"empty list", `{"connectionBytesReceived":[]}`, []float64{}, 0, false},
		{"a string element drops the list", `{"connectionBytesReceived":[1,"2"]}`, nil, 1, false},
		{"a nested element drops the list", `{"connectionBytesReceived":[1,[2]]}`, nil, 1, false},
		{"not a list", `{"connectionBytesReceived":12}`, nil, 1, false},
		{"null is absence", `{"connectionBytesReceived":null}`, nil, 0, false},
		{"width as a string", `{"dvrStripeWidth":"2"}`, nil, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var obj map[string]any
			dec := json.NewDecoder(strings.NewReader(c.in))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil {
				t.Fatal(err)
			}
			out, an := SanitizeStats(obj, ViewerFields)
			if an.Dropped != c.dropped || an.Unknown != 0 {
				t.Errorf("anomalies = %+v, want %d dropped and none unknown", an, c.dropped)
			}
			// Through the store and back, as the read path sees it.
			b, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var stored map[string]any
			if err := json.Unmarshal(b, &stored); err != nil {
				t.Fatal(err)
			}
			got, ok := Numbers(stored, "connectionBytesReceived")
			if (c.want != nil) != ok {
				t.Fatalf("Numbers ok = %v, want %v (stored %s)", ok, c.want != nil, b)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("element %d = %v, want %v", i, got[i], c.want[i])
				}
			}
			if _, ok := Number(stored, "dvrStripeWidth"); ok != c.widthSet {
				t.Errorf("dvrStripeWidth present = %v, want %v", ok, c.widthSet)
			}
		})
	}
}

func TestStripedDVRFieldsAreCatalogued(t *testing.T) {
	want := map[string]Semantic{"dvrStripeWidth": SemGauge, "connectionBytesReceived": SemObject}
	for _, f := range Catalogue() {
		if sem, ok := want[f.Name]; ok {
			if f.Semantic != sem || f.Desc == "" || len(f.Roles) != 1 || f.Roles[0] != "viewer" {
				t.Errorf("%s catalogued as %+v", f.Name, f)
			}
			delete(want, f.Name)
		}
	}
	for name := range want {
		t.Errorf("%s is not in the catalogue", name)
	}
}
