package notify

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"

	"github.com/Tuhis/gawk/gawk-server/events"
)

// The drift gates of docs/52 D7, gawk-admin side: the row vocabulary in
// internal/store, the row-type → CloudEvents-type table in gawk-server/events
// and the AsyncAPI catalogue's `webhook` channel are one story, and the D4
// projection of every golden vector is clean and still valid.

// compiler builds one validator over every embedded schema. Test-only: the
// production code never validates (D7), and the containment test below is
// what keeps it that way.
func compiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	files, err := events.SchemaFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, _ := events.SchemaByFile(name)
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("schema %s: %v", name, err)
		}
		if err := c.AddResource(events.SchemaIDBase+name, doc); err != nil {
			t.Fatalf("schema %s: %v", name, err)
		}
	}
	return c
}

// validateData asserts a delivery's data against the type's schema.
func validateData(t *testing.T, typ string, data map[string]any) {
	t.Helper()
	schema, err := compiler(t).Compile(events.SchemaID(typ))
	if err != nil {
		t.Fatalf("compiling the schema of %s: %v", typ, err)
	}
	// Through JSON so numbers are json.Number/float64 the way the validator
	// expects, not Go ints.
	raw, _ := json.Marshal(data)
	instance, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err := schema.Validate(instance); err != nil {
		t.Errorf("data of %s does not validate against %s: %v\n%s", typ, events.SchemaID(typ), err, raw)
	}
}

// catalogue is asyncapi.yaml as JSON.
func catalogue(t *testing.T) map[string]any {
	t.Helper()
	asJSON, err := yaml.YAMLToJSON(events.AsyncAPI)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(asJSON, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// channelTypes returns the CloudEvents type names listed under a channel.
func channelTypes(t *testing.T, doc map[string]any, channel string) []string {
	t.Helper()
	components, _ := doc["components"].(map[string]any)
	messages, _ := components["messages"].(map[string]any)
	channels, _ := doc["channels"].(map[string]any)
	ch, _ := channels[channel].(map[string]any)
	listed, _ := ch["messages"].(map[string]any)
	var out []string
	for _, entry := range listed {
		e, _ := entry.(map[string]any)
		ref, _ := e["$ref"].(string)
		msg, _ := messages[strings.TrimPrefix(ref, "#/components/messages/")].(map[string]any)
		name, _ := msg["name"].(string)
		if name == "" {
			t.Fatalf("channel %s references %q, which is no message", channel, ref)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestEveryRowTypeMapsToTheContract: store.AllEventTypes() and the events
// package's row-type table are the same set, so a row type added on either
// side without the other fails here.
func TestEveryRowTypeMapsToTheContract(t *testing.T) {
	rows := store.AllEventTypes()
	sort.Strings(rows)
	if table := events.ModerationRowTypes(); !slices.Equal(rows, table) {
		t.Fatalf("store.AllEventTypes() = %v\nevents.ModerationRowTypes() = %v\nthe two must be one table", rows, table)
	}
	for _, row := range rows {
		typ, ok := events.ModerationType(row)
		if !ok || !events.IsType(typ) {
			t.Errorf("row type %q maps to %q, %v", row, typ, ok)
		}
	}
}

// TestWebhookChannelIsWebhookEligibility is docs/52 D3's rule made a test:
// a message is webhook-eligible if and only if it is under the `webhook`
// channel, and that set — minus the synthetic test event, which is no row —
// is exactly the image of store.WebhookEventTypes().
func TestWebhookChannelIsWebhookEligibility(t *testing.T) {
	listed := channelTypes(t, catalogue(t), "webhook")
	listed = slices.DeleteFunc(listed, func(s string) bool { return s == events.TypeWebhookTest })

	var want []string
	for _, row := range store.WebhookEventTypes() {
		typ, ok := events.ModerationType(row)
		if !ok {
			t.Fatalf("WebhookEventTypes lists %q, which has no CloudEvents type", row)
		}
		want = append(want, typ)
	}
	sort.Strings(want)
	if !slices.Equal(listed, want) {
		t.Fatalf("asyncapi.yaml's webhook channel lists %v\nstore.WebhookEventTypes() maps to %v\nthe channel IS the eligibility rule (docs/52 D3): change both or neither", listed, want)
	}
	if slices.Contains(channelTypes(t, catalogue(t), "bus"), events.TypeWebhookTest) {
		t.Fatal("the test event is on the bus channel")
	}
}

// TestNoEventTypeIsARowType is docs/52 D6 (e): the store keeps its short
// internal names as row types, and no CloudEvents type — in particular no
// bus type's `<scope>.<event>` tokens — ever equals one of them. The docs/51
// D3 finding (`room.ended` meaning two shapes) can therefore not recur.
func TestNoEventTypeIsARowType(t *testing.T) {
	rows := map[string]bool{}
	for _, row := range store.AllEventTypes() {
		rows[row] = true
	}
	moderation := map[string]bool{}
	for _, row := range events.ModerationRowTypes() {
		typ, _ := events.ModerationType(row)
		moderation[typ] = true
	}
	for _, typ := range events.Types() {
		if rows[typ] {
			t.Errorf("%s is both a CloudEvents type and a moderation row type", typ)
		}
		if moderation[typ] {
			continue // a moderation type's tokens ARE its row type, by construction
		}
		if tokens := strings.TrimPrefix(typ, events.TypePrefix); rows[tokens] {
			t.Errorf("%s's tokens %q are a moderation row type: one string would mean two payload shapes", typ, tokens)
		}
	}
}

// TestEveryVectorProjectsClean is the D4 projection over every golden
// vector: the result carries no property the schema marks sensitive, still
// validates against the same schema, and — the R39 fixture assertion — no
// raw broadcast ID or room code from the vector survives into the body.
func TestEveryVectorProjectsClean(t *testing.T) {
	// The fictional identifiers the vectors carry, which a projected body
	// must not.
	vectorPoisons := []string{"ABC234", "R7K3MX", "tuhisroom"}
	for _, typ := range events.Types() {
		t.Run(typ, func(t *testing.T) {
			raw, err := events.Vector(typ)
			if err != nil {
				t.Fatal(err)
			}
			var full events.Event
			if err := json.Unmarshal(raw, &full); err != nil {
				t.Fatal(err)
			}
			projected, err := project(full, events.Delivery{
				Summary:   "one sentence for the receiver",
				PortalURL: "https://admin.example.com/#/broadcasts",
			})
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			body, err := events.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range vectorPoisons {
				if strings.Contains(string(body), p) {
					t.Errorf("the projection of %s leaked %q\n%s", typ, p, body)
				}
			}
			_, data := decode(t, body)
			assertProjected(t, typ, data)
			validateData(t, typ, data)
			if data["summary"] != "one sentence for the receiver" || data["portalUrl"] != "https://admin.example.com/#/broadcasts" {
				t.Errorf("the delivery-added properties were not filled: %s", body)
			}
			// The envelope is untouched: an intermediary, not a producer.
			var envelope map[string]any
			_ = json.Unmarshal(raw, &envelope)
			for _, attr := range []string{"id", "source", "type", "subject", "time", "dataschema"} {
				var got map[string]any
				_ = json.Unmarshal(body, &got)
				if got[attr] != envelope[attr] {
					t.Errorf("projection changed %s: %v → %v", attr, envelope[attr], got[attr])
				}
			}
		})
	}
}

// TestTheValidatorIsTestOnly is the containment rule of docs/52 D7 for this
// module: the JSON Schema validator is imported by _test.go files only, so
// the gawk-admin binary links no validator either.
func TestTheValidatorIsTestOnly(t *testing.T) {
	const moduleRoot = "../.."
	const forbidden = "github.com/santhosh-tekuri/jsonschema"
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != moduleRoot && (name == "vendor" || name == "node_modules" || name == "ui" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); strings.HasPrefix(p, forbidden) {
				rel, _ := filepath.Rel(moduleRoot, path)
				t.Errorf("%s imports %s: the validator is a test dependency (docs/52 D7); production code marshals typed structs and never validates", filepath.ToSlash(rel), p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d non-test Go files — the walk is not reaching the module tree", scanned)
	}
}
