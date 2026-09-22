package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"
)

// The drift gates of docs/52 D7, relay side. The contract is four things that
// must agree — the Type* constants, the schema files, the golden vectors and
// the AsyncAPI catalogue — plus the rules of D6 that only a test can hold
// anyone to. Everything is loaded into one in-memory `contract` value and
// checked by pure functions, so the negative cases below can mutate a copy
// and prove each check actually fires (the R48 OA1 shape).

// typePattern is D6 (a): reverse-DNS under fi.ioio.gawk, snake_case scope and
// event, an optional `.vN` suffix from v2 on.
var typePattern = regexp.MustCompile(`^fi\.ioio\.gawk\.[a-z_]+\.[a-z_]+(\.v[2-9][0-9]*)?$`)

// contract is everything the tests hold together.
type contract struct {
	types    []string
	schemas  map[string]json.RawMessage // by file name, common.json included
	vectors  map[string][]byte          // by type, the file's bytes
	fixtures map[string]Event
	catalog  map[string]any // asyncapi.yaml as JSON
	envelope json.RawMessage
	// now is the clock the sunset rule runs against.
	now time.Time
}

func load(t *testing.T) contract {
	t.Helper()
	c := contract{
		types:    Types(),
		schemas:  map[string]json.RawMessage{},
		vectors:  map[string][]byte{},
		fixtures: fixtures(),
		envelope: CloudEventsSchema,
		now:      time.Now(),
	}
	files, err := SchemaFiles()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	for _, name := range files {
		raw, ok := SchemaByFile(name)
		if !ok {
			t.Fatalf("schema %s listed but unreadable", name)
		}
		c.schemas[name] = raw
	}
	entries, err := os.ReadDir(filepath.Join("testdata", "vectors"))
	if err != nil {
		t.Fatalf("listing vectors: %v", err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", "vectors", e.Name()))
		if err != nil {
			t.Fatalf("reading vector %s: %v", e.Name(), err)
		}
		c.vectors[strings.TrimSuffix(e.Name(), ".json")] = raw
	}
	asJSON, err := yaml.YAMLToJSON(AsyncAPI)
	if err != nil {
		t.Fatalf("asyncapi.yaml is not valid YAML: %v", err)
	}
	if err := json.Unmarshal(asJSON, &c.catalog); err != nil {
		t.Fatalf("asyncapi.yaml is not an object: %v", err)
	}
	return c
}

// clone is a deep enough copy for the negative cases to mutate.
func (c contract) clone() contract {
	out := c
	out.types = append([]string(nil), c.types...)
	out.schemas = map[string]json.RawMessage{}
	for k, v := range c.schemas {
		out.schemas[k] = append(json.RawMessage(nil), v...)
	}
	out.vectors = map[string][]byte{}
	for k, v := range c.vectors {
		out.vectors[k] = append([]byte(nil), v...)
	}
	out.fixtures = map[string]Event{}
	for k, v := range c.fixtures {
		out.fixtures[k] = v
	}
	raw, _ := json.Marshal(c.catalog)
	out.catalog = nil
	_ = json.Unmarshal(raw, &out.catalog)
	return out
}

func TestContract(t *testing.T) {
	c := load(t)

	t.Run("the four sets agree", func(t *testing.T) { report(t, c.checkSets()) })
	t.Run("types follow the naming rule", func(t *testing.T) { report(t, c.checkNames()) })
	t.Run("schemas are open, identified and mark nothing sensitive as required", func(t *testing.T) {
		report(t, c.checkSchemas())
	})
	t.Run("every vector is a CloudEvent, validates against its schema and is its fixture's bytes", func(t *testing.T) {
		report(t, c.checkVectors())
	})
	t.Run("the catalogue lists every type once, on the right channels, with a lifecycle", func(t *testing.T) {
		report(t, c.checkCatalog())
	})

	// The negative cases: each is a mutation somebody could plausibly make
	// by mistake, and each must be caught by name. Without these, a check
	// that silently found nothing would pass forever.
	t.Run("negative: a deleted schema file fails", func(t *testing.T) {
		m := c.clone()
		delete(m.schemas, SchemaFile(TypeRoomAttached))
		mustFail(t, m.checkSets(), "schema/fi.ioio.gawk.room.attached.json")
	})
	t.Run("negative: a deleted vector fails", func(t *testing.T) {
		m := c.clone()
		delete(m.vectors, TypeRoomAttached)
		mustFail(t, m.checkSets(), "testdata/vectors/fi.ioio.gawk.room.attached.json")
	})
	t.Run("negative: a deleted AsyncAPI message fails", func(t *testing.T) {
		m := c.clone()
		delete(m.messages(), "roomAttached")
		mustFail(t, m.checkSets(), "no AsyncAPI message named fi.ioio.gawk.room.attached")
	})
	t.Run("negative: a deleted Type constant fails", func(t *testing.T) {
		m := c.clone()
		m.types = without(m.types, TypeRoomAttached)
		mustFail(t, m.checkSets(), "fi.ioio.gawk.room.attached has a schema file but no Type* constant")
	})
	t.Run("negative: additionalProperties: false fails", func(t *testing.T) {
		m := c.clone()
		m.schemas[SchemaFile(TypeRoomAttached)] = withKey(t, m.schemas[SchemaFile(TypeRoomAttached)], "additionalProperties", false)
		mustFail(t, m.checkSchemas(), "additionalProperties: false")
		// And the vector — a fully-populated event that then carries the
		// delivery-added properties — would still validate, so the schema
		// rule is the only thing that catches it. Prove the schema rule is
		// what fires by checking the closed schema also refuses a projected
		// delivery: summary and portalUrl are declared, so it passes there;
		// the point of the rule is a consumer's OLD schema, not ours.
	})
	t.Run("negative: a sensitive property listed as required fails", func(t *testing.T) {
		m := c.clone()
		m.schemas[SchemaFile(TypeRoomAttached)] = withKey(t, m.schemas[SchemaFile(TypeRoomAttached)], "required",
			[]string{"roomKey", "broadcastKey", "broadcastId"})
		mustFail(t, m.checkSchemas(), "broadcastId is x-gawk-sensitive and required")
	})
	t.Run("negative: a fixture that drifts from its vector fails", func(t *testing.T) {
		m := c.clone()
		ev := m.fixtures[TypeRoomAttached]
		ev.Subject = "0000000000ff"
		m.fixtures[TypeRoomAttached] = ev
		mustFail(t, m.checkVectors(), "fixture for fi.ioio.gawk.room.attached does not marshal to its vector")
	})
	t.Run("negative: a vector whose data breaks its schema fails", func(t *testing.T) {
		m := c.clone()
		m.vectors[TypeRoomAttached] = bytes.Replace(m.vectors[TypeRoomAttached],
			[]byte(`"broadcastId": "ABC234"`), []byte(`"broadcastId": "abc-234"`), 1)
		mustFail(t, m.checkVectors(), "data of fi.ioio.gawk.room.attached does not validate")
	})
	t.Run("negative: a deprecated message without a sunset fails", func(t *testing.T) {
		m := c.clone()
		m.messages()["roomAttached"].(map[string]any)["x-gawk-status"] = "deprecated"
		mustFail(t, m.checkCatalog(), "roomAttached is deprecated but has no x-gawk-sunset")
	})
	t.Run("negative: a sunset in the past fails, by design", func(t *testing.T) {
		m := c.clone()
		msg := m.messages()["roomAttached"].(map[string]any)
		msg["x-gawk-status"] = "deprecated"
		msg["x-gawk-sunset"] = m.now.AddDate(0, 0, -1).Format("2006-01-02")
		mustFail(t, m.checkCatalog(), "sunset has passed: open the removal PR")
		// …and one still in the future is fine: this is what a deprecation
		// window looks like while it is open.
		msg["x-gawk-sunset"] = m.now.AddDate(0, 0, 90).Format("2006-01-02")
		if errs := m.checkCatalog(); len(errs) != 0 {
			t.Fatalf("a future sunset should pass: %v", errs)
		}
	})
	t.Run("negative: a bus type reusing a moderation row type's tokens fails", func(t *testing.T) {
		m := c.clone()
		// Rename the bus message roomClosed to fi.ioio.gawk.room.ended —
		// the docs/51 D3 finding: one string, two payload shapes.
		m.messages()["roomClosed"].(map[string]any)["name"] = TypeRoomEnded
		mustFail(t, m.checkCatalog(), "room.ended is a moderation row type")
	})
	t.Run("negative: a webhook-only type on the bus fails", func(t *testing.T) {
		m := c.clone()
		m.channelMessages("bus")["webhookTest"] = map[string]any{"$ref": "#/components/messages/webhookTest"}
		mustFail(t, m.checkCatalog(), "fi.ioio.gawk.webhook.test is listed on the bus channel")
	})
}

// ---------------------------------------------------------------------------
// the checks
// ---------------------------------------------------------------------------

// checkSets: Types() ⇔ schema files ⇔ vectors ⇔ AsyncAPI messages ⇔ fixtures.
func (c contract) checkSets() []error {
	var errs []error
	types := map[string]bool{}
	for _, tp := range c.types {
		if types[tp] {
			errs = append(errs, fmt.Errorf("Types() lists %s twice", tp))
		}
		types[tp] = true
		if _, ok := c.schemas[SchemaFile(tp)]; !ok {
			errs = append(errs, fmt.Errorf("%s has no schema/%s", tp, SchemaFile(tp)))
		}
		if _, ok := c.vectors[tp]; !ok {
			errs = append(errs, fmt.Errorf("%s has no testdata/vectors/%s.json", tp, tp))
		}
		if _, ok := c.fixtures[tp]; !ok {
			errs = append(errs, fmt.Errorf("%s has no Go fixture in fixtures_test.go", tp))
		}
		if _, ok := c.messageByName(tp); !ok {
			errs = append(errs, fmt.Errorf("no AsyncAPI message named %s in asyncapi.yaml", tp))
		}
	}
	for name := range c.schemas {
		if name == CommonSchemaFile {
			continue
		}
		if tp := strings.TrimSuffix(name, ".json"); !types[tp] {
			errs = append(errs, fmt.Errorf("%s has a schema file but no Type* constant in Types()", tp))
		}
	}
	if _, ok := c.schemas[CommonSchemaFile]; !ok {
		errs = append(errs, errors.New("schema/common.json is missing"))
	}
	for tp := range c.vectors {
		if !types[tp] {
			errs = append(errs, fmt.Errorf("testdata/vectors/%s.json has no Type* constant", tp))
		}
	}
	for tp := range c.fixtures {
		if !types[tp] {
			errs = append(errs, fmt.Errorf("fixture %s has no Type* constant", tp))
		}
	}
	seen := map[string]string{}
	for key, msg := range c.messages() {
		m, _ := msg.(map[string]any)
		name, _ := m["name"].(string)
		if !types[name] {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s is named %q, which is no Type* constant", key, name))
		}
		if prev, dup := seen[name]; dup {
			errs = append(errs, fmt.Errorf("AsyncAPI messages %s and %s are both named %s", prev, key, name))
		}
		seen[name] = key
	}
	sortErrs(errs)
	return errs
}

// checkNames is D6 (a): the reverse-DNS shape, and Scope/Name agreeing with
// it.
func (c contract) checkNames() []error {
	var errs []error
	for _, tp := range c.types {
		if !typePattern.MatchString(tp) {
			errs = append(errs, fmt.Errorf("%s does not match %s (D6 a)", tp, typePattern))
			continue
		}
		if Scope(tp) == "" || Name(tp) == "" {
			errs = append(errs, fmt.Errorf("Scope/Name cannot split %s", tp))
		}
		if !IsType(tp) {
			errs = append(errs, fmt.Errorf("IsType(%s) is false", tp))
		}
	}
	for row, tp := range moderationTypes {
		if !IsType(tp) {
			errs = append(errs, fmt.Errorf("ModerationType(%q) = %s, which is not in Types()", row, tp))
		}
		if TypePrefix+row != tp {
			errs = append(errs, fmt.Errorf("ModerationType(%q) = %s: a moderation row type maps to its own tokens under the prefix", row, tp))
		}
	}
	sortErrs(errs)
	return errs
}

// checkSchemas is D2 + D4 + D6 (b) as file rules: `$id` and `$schema`, no
// closed objects anywhere, no sensitive property required, every `$ref`
// pointing at a file that exists.
func (c contract) checkSchemas() []error {
	var errs []error
	names := make([]string, 0, len(c.schemas))
	for name := range c.schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var doc map[string]any
		if err := json.Unmarshal(c.schemas[name], &doc); err != nil {
			errs = append(errs, fmt.Errorf("schema/%s is not JSON: %v", name, err))
			continue
		}
		if got := doc["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
			errs = append(errs, fmt.Errorf("schema/%s declares $schema %v, want 2020-12 (D2)", name, got))
		}
		if got := doc["$id"]; got != SchemaIDBase+name {
			errs = append(errs, fmt.Errorf("schema/%s declares $id %v, want %s (D2)", name, got, SchemaIDBase+name))
		}
		walkSchema(doc, "", func(path string, node map[string]any) {
			if v, ok := node["additionalProperties"]; ok && v == false {
				errs = append(errs, fmt.Errorf("schema/%s%s sets additionalProperties: false; schemas stay open so an additive revision keeps validating (D6 b)", name, path))
			}
			if ref, ok := node["$ref"].(string); ok && !strings.HasPrefix(ref, "#") {
				file, _, _ := strings.Cut(ref, "#")
				if _, exists := c.schemas[file]; !exists {
					errs = append(errs, fmt.Errorf("schema/%s%s $refs %q, which is not a schema file here", name, path, ref))
				}
			}
		})
		if name == CommonSchemaFile {
			continue
		}
		props, _ := doc["properties"].(map[string]any)
		required, _ := doc["required"].([]any)
		for _, r := range required {
			rn, _ := r.(string)
			p, _ := props[rn].(map[string]any)
			if p == nil {
				errs = append(errs, fmt.Errorf("schema/%s requires %q, which it does not declare", name, rn))
				continue
			}
			if s, _ := p["x-gawk-sensitive"].(bool); s {
				errs = append(errs, fmt.Errorf("schema/%s: %s is x-gawk-sensitive and required; a producer may not know a joinable identifier — an IP ban names no broadcast, an unhomed room has no key — and an event it cannot fill is not an event to drop (D4)", name, rn))
			}
		}
		for _, delivery := range []string{"summary", "portalUrl"} {
			if _, ok := props[delivery]; !ok {
				errs = append(errs, fmt.Errorf("schema/%s does not declare %s; every data schema declares both delivery-added properties (D4)", name, delivery))
			}
		}
	}
	sortErrs(errs)
	return errs
}

// checkVectors is D7's heart: every vector is a CloudEvent by the project's
// own envelope schema, its data validates against its type's schema, its
// attributes are the ones the type implies, and the Go fixture reproduces
// the file byte for byte in both renderings.
func (c contract) checkVectors() []error {
	var errs []error
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	for name, raw := range c.schemas {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return []error{fmt.Errorf("schema/%s: %v", name, err)}
		}
		if err := compiler.AddResource(SchemaIDBase+name, doc); err != nil {
			return []error{fmt.Errorf("schema/%s: %v", name, err)}
		}
	}
	const envelopeURL = "https://github.com/cloudevents/spec/cloudevents/formats/cloudevents.json"
	envDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(c.envelope))
	if err != nil {
		return []error{fmt.Errorf("cloudevents.json: %v", err)}
	}
	if err := compiler.AddResource(envelopeURL, envDoc); err != nil {
		return []error{fmt.Errorf("cloudevents.json: %v", err)}
	}
	envelope, err := compiler.Compile(envelopeURL)
	if err != nil {
		return []error{fmt.Errorf("compiling cloudevents.json: %v", err)}
	}

	for _, tp := range c.types {
		raw, ok := c.vectors[tp]
		if !ok {
			continue // checkSets reports it
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			errs = append(errs, fmt.Errorf("vector %s is not JSON: %v", tp, err))
			continue
		}
		if err := envelope.Validate(instance); err != nil {
			errs = append(errs, fmt.Errorf("vector %s is not a CloudEvent: %v", tp, err))
		}
		obj, _ := instance.(map[string]any)
		for attr, want := range map[string]string{
			"specversion": SpecVersion, "type": tp, "datacontenttype": DataContentType, "dataschema": SchemaID(tp),
		} {
			if got := obj[attr]; got != want {
				errs = append(errs, fmt.Errorf("vector %s: %s = %v, want %s", tp, attr, got, want))
			}
		}
		if src, _ := obj["source"].(string); src != SourceAdmin && !strings.HasPrefix(src, sourceRelayPrefix) {
			errs = append(errs, fmt.Errorf("vector %s: source %q is neither %s nor %s<pod>", tp, src, SourceAdmin, sourceRelayPrefix))
		}
		schema, err := compiler.Compile(SchemaID(tp))
		if err != nil {
			errs = append(errs, fmt.Errorf("compiling schema/%s: %v", SchemaFile(tp), err))
			continue
		}
		if err := schema.Validate(obj["data"]); err != nil {
			errs = append(errs, fmt.Errorf("data of %s does not validate against its schema: %v", tp, err))
		}
		// Fully populated: every declared property is present in the
		// vector, except the two delivery-added ones on a bus event, which
		// the relay never sets — so the vector shows what the relay sends.
		var decl struct {
			Properties map[string]any `json:"properties"`
		}
		_ = json.Unmarshal(c.schemas[SchemaFile(tp)], &decl)
		data, _ := obj["data"].(map[string]any)
		for prop := range decl.Properties {
			if _, present := data[prop]; present {
				continue
			}
			if (prop == "summary" || prop == "portalUrl") && strings.HasPrefix(obj["source"].(string), sourceRelayPrefix) {
				continue
			}
			errs = append(errs, fmt.Errorf("vector %s does not populate %s; a golden vector carries every property", tp, prop))
		}
		for prop := range data {
			if _, declared := decl.Properties[prop]; !declared {
				errs = append(errs, fmt.Errorf("vector %s carries %s, which its schema does not declare", tp, prop))
			}
		}

		fixture, ok := c.fixtures[tp]
		if !ok {
			continue
		}
		wire, err := Marshal(fixture)
		if err != nil {
			errs = append(errs, fmt.Errorf("marshal fixture %s: %v", tp, err))
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			errs = append(errs, fmt.Errorf("vector %s: %v", tp, err))
			continue
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, wire, "", "  ")
		pretty.WriteByte('\n')
		if !bytes.Equal(compact.Bytes(), wire) || !bytes.Equal(pretty.Bytes(), raw) {
			errs = append(errs, fmt.Errorf("fixture for %s does not marshal to its vector\n got: %s\nwant: %s", tp, wire, compact.Bytes()))
		}
	}
	sortErrs(errs)
	return errs
}

// checkCatalog is D3 + D6 (c)–(e) on asyncapi.yaml.
func (c contract) checkCatalog() []error {
	var errs []error
	if v, _ := c.catalog["asyncapi"].(string); !strings.HasPrefix(v, "3.") {
		errs = append(errs, fmt.Errorf("asyncapi.yaml declares asyncapi %q, want 3.x", v))
	}
	messages := c.messages()
	keys := make([]string, 0, len(messages))
	for k := range messages {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	bus := c.channelTypes("bus")
	webhook := c.channelTypes("webhook")
	referenced := map[string]bool{}
	for _, byKey := range []map[string]string{bus, webhook} {
		for key := range byKey {
			referenced[key] = true
		}
	}

	for _, key := range keys {
		msg, _ := messages[key].(map[string]any)
		name, _ := msg["name"].(string)
		if !referenced[key] {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s (%s) is on no channel; a message nothing emits is dead", key, name))
		}
		if since, _ := msg["x-gawk-since"].(string); !regexp.MustCompile(`^R[0-9]+$`).MatchString(since) {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s has x-gawk-since %q, want the milestone that shipped it (D6 d)", key, since))
		}
		status, _ := msg["x-gawk-status"].(string)
		switch status {
		case "stable":
			if _, has := msg["x-gawk-sunset"]; has {
				errs = append(errs, fmt.Errorf("AsyncAPI message %s is stable but carries an x-gawk-sunset", key))
			}
		case "deprecated":
			sunset, _ := msg["x-gawk-sunset"].(string)
			if sunset == "" {
				errs = append(errs, fmt.Errorf("AsyncAPI message %s is deprecated but has no x-gawk-sunset (D6 c)", key))
				break
			}
			day, err := time.Parse("2006-01-02", sunset)
			if err != nil {
				errs = append(errs, fmt.Errorf("AsyncAPI message %s: x-gawk-sunset %q is not a YYYY-MM-DD date", key, sunset))
				break
			}
			if !day.After(c.now) {
				errs = append(errs, fmt.Errorf("AsyncAPI message %s: sunset has passed: open the removal PR for %s, or move the sunset with a dated note (D7)", key, name))
			}
		default:
			errs = append(errs, fmt.Errorf("AsyncAPI message %s has x-gawk-status %q, want stable or deprecated", key, status))
		}
		if ct, _ := msg["contentType"].(string); ct != "" && ct != ContentType {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s has contentType %q, want %s", key, ct, ContentType))
		}
		// payload = the envelope with `type` fixed and `data` $ref'ing the
		// D2 file.
		payload, _ := msg["payload"].(map[string]any)
		allOf, _ := payload["allOf"].([]any)
		if len(allOf) != 2 {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s: payload is not allOf[envelope, {type, data}]", key))
			continue
		}
		if env, _ := allOf[0].(map[string]any); env["$ref"] != "#/components/schemas/cloudEvent" {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s: payload.allOf[0] does not $ref the envelope", key))
		}
		own, _ := allOf[1].(map[string]any)
		props, _ := own["properties"].(map[string]any)
		typ, _ := props["type"].(map[string]any)
		if typ["const"] != name {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s: payload fixes type %v, want %s", key, typ["const"], name))
		}
		data, _ := props["data"].(map[string]any)
		if want := "./" + SchemaDir + "/" + SchemaFile(name); data["$ref"] != want {
			errs = append(errs, fmt.Errorf("AsyncAPI message %s: data $refs %v, want %s", key, data["$ref"], want))
		}
	}

	// Channel membership (D3, D6 e): every bus type is a relay type whose
	// tokens are no moderation row type; the moderation types are webhook
	// messages and never bus messages; the test type is webhook-only.
	moderation := map[string]bool{}
	for _, tp := range moderationTypes {
		moderation[tp] = true
	}
	rows := map[string]bool{}
	for _, row := range ModerationRowTypes() {
		rows[row] = true
	}
	for key, tp := range bus {
		if moderation[tp] {
			errs = append(errs, fmt.Errorf("bus channel lists %s (%s), a portal-originated moderation event", key, tp))
		}
		if tp == TypeWebhookTest {
			errs = append(errs, fmt.Errorf("%s is listed on the bus channel; it is webhook-only", tp))
		}
		if tokens := strings.TrimPrefix(tp, TypePrefix); rows[tokens] {
			errs = append(errs, fmt.Errorf("bus message %s is named %s, but %s is a moderation row type: one string would mean two payload shapes (docs/51 D3, D6 e)", key, tp, tokens))
		}
	}
	webhookTypes := map[string]bool{}
	for _, tp := range webhook {
		webhookTypes[tp] = true
	}
	for row, tp := range moderationTypes {
		if !webhookTypes[tp] {
			errs = append(errs, fmt.Errorf("webhook channel does not list %s (row type %s); every moderation event pages someone", tp, row))
		}
	}
	if !webhookTypes[TypeWebhookTest] {
		errs = append(errs, fmt.Errorf("webhook channel does not list %s", TypeWebhookTest))
	}
	// The bus address's scope enum must admit every scope the bus carries.
	if scopes := c.busScopes(); scopes != nil {
		for _, tp := range bus {
			if !scopes[Scope(tp)] {
				errs = append(errs, fmt.Errorf("bus channel carries %s but its {scope} parameter enum lacks %q", tp, Scope(tp)))
			}
		}
	}
	sortErrs(errs)
	return errs
}

// ---------------------------------------------------------------------------
// catalogue navigation
// ---------------------------------------------------------------------------

func (c contract) messages() map[string]any {
	components, _ := c.catalog["components"].(map[string]any)
	messages, _ := components["messages"].(map[string]any)
	return messages
}

func (c contract) messageByName(name string) (string, bool) {
	for key, msg := range c.messages() {
		m, _ := msg.(map[string]any)
		if m["name"] == name {
			return key, true
		}
	}
	return "", false
}

func (c contract) channelMessages(channel string) map[string]any {
	channels, _ := c.catalog["channels"].(map[string]any)
	ch, _ := channels[channel].(map[string]any)
	msgs, _ := ch["messages"].(map[string]any)
	return msgs
}

// channelTypes resolves a channel's message $refs to component keys → type
// names.
func (c contract) channelTypes(channel string) map[string]string {
	out := map[string]string{}
	for _, m := range c.channelMessages(channel) {
		entry, _ := m.(map[string]any)
		ref, _ := entry["$ref"].(string)
		key := strings.TrimPrefix(ref, "#/components/messages/")
		msg, _ := c.messages()[key].(map[string]any)
		name, _ := msg["name"].(string)
		out[key] = name
	}
	return out
}

func (c contract) busScopes() map[string]bool {
	channels, _ := c.catalog["channels"].(map[string]any)
	bus, _ := channels["bus"].(map[string]any)
	params, _ := bus["parameters"].(map[string]any)
	scope, _ := params["scope"].(map[string]any)
	enum, _ := scope["enum"].([]any)
	if enum == nil {
		return nil
	}
	out := map[string]bool{}
	for _, v := range enum {
		s, _ := v.(string)
		out[s] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// walkSchema visits every object in a schema document, depth first.
func walkSchema(node map[string]any, path string, visit func(string, map[string]any)) {
	visit(path, node)
	for k, v := range node {
		switch child := v.(type) {
		case map[string]any:
			walkSchema(child, path+"/"+k, visit)
		case []any:
			for i, item := range child {
				if m, ok := item.(map[string]any); ok {
					walkSchema(m, fmt.Sprintf("%s/%s/%d", path, k, i), visit)
				}
			}
		}
	}
}

func withKey(t *testing.T, raw json.RawMessage, key string, value any) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func without(list []string, drop string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func report(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		t.Error(err)
	}
}

func mustFail(t *testing.T, errs []error, what string) {
	t.Helper()
	for _, err := range errs {
		if strings.Contains(err.Error(), what) {
			return
		}
	}
	t.Fatalf("expected a failure mentioning %q, got %v", what, errs)
}

func sortErrs(errs []error) {
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
}

// ---------------------------------------------------------------------------
// the encoder and the small helpers
// ---------------------------------------------------------------------------

func TestMarshalIsTheWireEncoder(t *testing.T) {
	ev := New(TypeRoomAttached, BusID("relay-0", 7), SourceRelay("relay-0"), fixtureRoomKey,
		time.Date(2026, 9, 17, 14, 0, 0, 0, time.FixedZone("EEST", 3*3600)),
		RoomAttachedData{RoomKey: fixtureRoomKey, BroadcastKey: fixtureBroadcastKey, Label: "a & b <c>"})
	out, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.HasSuffix(s, "\n") {
		t.Error("Marshal emitted a trailing newline; the signed material is the JSON value itself")
	}
	if !strings.Contains(s, `"label":"a & b <c>"`) {
		t.Errorf("Marshal HTML-escaped human text: %s", s)
	}
	if !strings.Contains(s, `"time":"2026-09-17T11:00:00Z"`) {
		t.Errorf("Marshal did not normalise time to UTC: %s", s)
	}
	if !strings.Contains(s, `"id":"relay-0:7"`) || !strings.Contains(s, `"source":"/gawk/relay/relay-0"`) {
		t.Errorf("BusID/SourceRelay shape: %s", s)
	}
	if !strings.Contains(s, `"dataschema":"`+SchemaIDBase+`fi.ioio.gawk.room.attached.json"`) {
		t.Errorf("dataschema: %s", s)
	}
	if strings.Contains(s, `"summary"`) || strings.Contains(s, `"portalUrl"`) {
		t.Errorf("an unset delivery property was emitted: %s", s)
	}
	// Attribute order is the JSON format's, so `nats sub` output reads as
	// the event and a diff of two vectors lines up.
	if !strings.HasPrefix(s, `{"specversion":"1.0","id":`) {
		t.Errorf("attribute order changed: %s", s)
	}
	// Subject is omitted, not empty, when there is none.
	none, _ := Marshal(New(TypeWebhookTest, "x", SourceAdmin, "", time.Now(), WebhookTestData{}))
	if strings.Contains(string(none), `"subject"`) {
		t.Errorf("an empty subject was emitted: %s", none)
	}
}

func TestTypeHelpers(t *testing.T) {
	if Scope(TypeRoomParticipantJoined) != "room" || Name(TypeRoomParticipantJoined) != "participant_joined" {
		t.Errorf("Scope/Name of %s = %q/%q", TypeRoomParticipantJoined, Scope(TypeRoomParticipantJoined), Name(TypeRoomParticipantJoined))
	}
	if Name("fi.ioio.gawk.room.participant_joined.v2") != "participant_joined.v2" {
		t.Error("a versioned type keeps its suffix in Name")
	}
	for _, bad := range []string{"", "room.ended", "fi.ioio.gawk.room", "com.example.x.y"} {
		if Scope(bad) != "" || Name(bad) != "" || IsType(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
	if tp, ok := ModerationType("room.ended"); !ok || tp != TypeRoomEnded {
		t.Errorf("ModerationType(room.ended) = %q, %v", tp, ok)
	}
	if _, ok := ModerationType("room.closed"); ok {
		t.Error("room.closed is a bus type, not a moderation row type")
	}
	if _, ok := SchemaByFile("../events.go"); ok {
		t.Error("SchemaByFile followed a path")
	}
	if _, ok := SchemaByFile("nope.json"); ok {
		t.Error("SchemaByFile found a schema that does not exist")
	}
	if raw, ok := SchemaByFile(CommonSchemaFile); !ok || len(raw) == 0 {
		t.Error("SchemaByFile cannot read common.json")
	}
	sensitive, err := SensitiveProperties(TypeRoomAttached)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sensitive, ",") != "broadcastId,displayCode,roomCode" {
		t.Errorf("SensitiveProperties(room.attached) = %v", sensitive)
	}
	if _, err := SensitiveProperties("fi.ioio.gawk.nope.nope"); err == nil {
		t.Error("SensitiveProperties of an unknown type did not fail")
	}
}

// TestSubjectIsTheCleartextIdentity is docs/52 D9 over every golden vector:
// `subject` is the raw broadcast ID or the room code carried in `data`, never
// the HMAC'd key — the identity a person typed to join, so a consumer can
// route, group and render events without resolving a digest first.
//
// It reads the vectors rather than the fixtures on purpose: the vector is the
// document a consumer is shown, and the fixture is already held byte-identical
// to it by TestContract.
func TestSubjectIsTheCleartextIdentity(t *testing.T) {
	for _, typ := range Types() {
		t.Run(typ, func(t *testing.T) {
			raw, err := Vector(typ)
			if err != nil {
				t.Fatal(err)
			}
			var ev struct {
				Subject string `json:"subject"`
				Data    struct {
					BroadcastID  string `json:"broadcastId"`
					BroadcastKey string `json:"broadcastKey"`
					RoomCode     string `json:"roomCode"`
					RoomKey      string `json:"roomKey"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatal(err)
			}
			// A room event is about its room even when it also names a
			// broadcast; a broadcast event is about the broadcast.
			want := ev.Data.RoomCode
			if want == "" {
				want = ev.Data.BroadcastID
			}
			if ev.Subject != want {
				t.Errorf("subject = %q, want the cleartext %q (D9)", ev.Subject, want)
			}
			for _, key := range []string{ev.Data.RoomKey, ev.Data.BroadcastKey} {
				if key != "" && ev.Subject == key {
					t.Errorf("subject = %q, which is the HMAC'd key; D9 made it the cleartext identity", ev.Subject)
				}
			}
		})
	}
}
