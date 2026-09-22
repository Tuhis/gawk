// Package storedshape enforces docs/33 D4's "additive forever" on everything
// the service writes to disk.
//
// The rule was written down on day one — fields are appended, never renamed or
// repurposed — and nothing checked it. It matters more than it looks, because
// the SQL views read the stored NDJSON with `union_by_name`: a field that is a
// number in one partition and a string in another does not fail anywhere at
// write time. It turns the whole column into JSON for every query over every
// partition, so `avg(x)` stops working on history nobody can rewrite (rollups
// are permanent). A removed or renamed field is the same failure in slower
// motion: its old rows keep the old name forever.
//
// So the shape of every stored row type is flattened into a golden file
// (testdata/stored-shape.golden), and the test fails when a recorded field
// changes type or disappears. A NEW field fails too, until it is recorded —
// that is what makes the golden complete enough to catch the next change to
// it. Recording is append-only: GAWK_UPDATE_GOLDEN=1 adds new fields and never
// rewrites a recorded one, so a breaking change can only land by hand-editing
// the golden, where a reviewer sees it.
package storedshape

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
)

// JSON types as the views see them. Numbers are one type on purpose: an int in
// one partition and a float in another union to DOUBLE, which is harmless.
const (
	String  = "string"
	Number  = "number"
	Boolean = "boolean"
	Object  = "object"
	Map     = "map"
	Array   = "array"
	Any     = "any"
	// Custom is a type with its own MarshalJSON: its JSON shape is not visible
	// to reflection, so it is recorded but cannot be checked field by field.
	Custom = "custom"
)

var (
	timeType      = reflect.TypeOf(time.Time{})
	rawType       = reflect.TypeOf(json.RawMessage(nil))
	marshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
)

// Of flattens t, as encoding/json marshals it, into path → JSON type. Struct
// fields are "a.b", map values "a.*", slice elements "a[]".
func Of(prefix string, t reflect.Type) map[string]string {
	out := map[string]string{}
	walk(out, prefix, t, map[reflect.Type]bool{})
	// The row itself is not a field of anything.
	if out[prefix] == Object {
		delete(out, prefix)
	}
	return out
}

func walk(out map[string]string, path string, t reflect.Type, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		out[path] = String
		return
	case t == rawType:
		// Pre-marshaled JSON; the caller records what goes into it separately.
		out[path] = Any
		return
	case t.Implements(marshalerType) || reflect.PointerTo(t).Implements(marshalerType):
		out[path] = Custom
		return
	}
	switch t.Kind() {
	case reflect.String:
		out[path] = String
	case reflect.Bool:
		out[path] = Boolean
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		out[path] = Number
	case reflect.Interface:
		out[path] = Any
	case reflect.Map:
		out[path] = Map
		walk(out, path+".*", t.Elem(), seen)
	case reflect.Slice, reflect.Array:
		out[path] = Array
		walk(out, path+"[]", t.Elem(), seen)
	case reflect.Struct:
		if path != "" {
			out[path] = Object
		}
		if seen[t] {
			return
		}
		seen[t] = true
		defer delete(seen, t)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" && opts == "" {
				continue
			}
			// An untagged embedded struct's fields are promoted, as
			// encoding/json does.
			if f.Anonymous && name == "" {
				walk(out, path, f.Type, seen)
				continue
			}
			if name == "" {
				name = f.Name
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
			if strings.Contains(","+opts+",", ",string,") {
				out[child] = String
				continue
			}
			walk(out, child, f.Type, seen)
		}
	default:
		out[path] = fmt.Sprintf("unsupported:%s", t.Kind())
	}
}

// Diff compares the current shape against the recorded one.
type Diff struct {
	// Changed are "path: was → now". Never fixable by re-recording.
	Changed []string
	// Removed are recorded paths the code no longer writes.
	Removed []string
	// Added are paths the code writes that are not recorded yet.
	Added []string
}

// Compare diffs current against golden.
func Compare(golden, current map[string]string) Diff {
	var d Diff
	for p, was := range golden {
		now, ok := current[p]
		switch {
		case !ok:
			d.Removed = append(d.Removed, p)
		case now != was:
			d.Changed = append(d.Changed, fmt.Sprintf("%s: %s → %s", p, was, now))
		}
	}
	for p, t := range current {
		if _, ok := golden[p]; !ok {
			d.Added = append(d.Added, p+" "+t)
		}
	}
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	sort.Strings(d.Added)
	return d
}

// Parse reads a golden file: one "path type" per line, # comments allowed.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, typ, ok := strings.Cut(line, " ")
		if !ok || strings.ContainsAny(strings.TrimSpace(typ), " \t") {
			return nil, fmt.Errorf("golden line %d: want \"path type\", got %q", n, line)
		}
		if _, dup := out[path]; dup {
			return nil, fmt.Errorf("golden line %d: %s recorded twice", n, path)
		}
		out[path] = strings.TrimSpace(typ)
	}
	return out, sc.Err()
}

// Format renders a shape as golden lines, sorted, under header.
func Format(header string, m map[string]string) []byte {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(header)
	for _, p := range paths {
		fmt.Fprintf(&b, "%s %s\n", p, m[p])
	}
	return []byte(b.String())
}
