package sqlengine

// Column types as DuckDB's JSON sniffer writes them, and how two of them merge.
//
// The views read the store with an explicit column list (learn.go) instead of
// `union_by_name`, which re-sniffed every file on every bind. That list is
// learned a few files at a time, and merging what one batch of files says with
// what the batches before it said is this file's job.
//
// DuckDB's own merge cannot be borrowed. For JSON, `union_by_name` does not
// fold per-file types: it sniffs every file into one internal structure and
// converts that to types once (JSONScan::AutoDetect, v1.4.1), and nothing
// exposes that step. So merge mirrors what it produces — pinned case by case
// against the real engine in learn_test.go, so a DuckDB upgrade that changes
// the answer fails CI rather than drifting:
//
//	BIGINT ⊔ DOUBLE                      → DOUBLE
//	DATE ⊔ TIMESTAMP ⊔ VARCHAR           → VARCHAR (all strings on disk)
//	any other two different scalars      → JSON
//	STRUCT ⊔ STRUCT                      → union of fields by name, recursively
//	LIST ⊔ LIST, MAP ⊔ MAP               → merge of the element / value type
//	nested ⊔ scalar, STRUCT ⊔ LIST       → JSON
//	null-only, `[]`, `{}` ⊔ anything     → the other side
//
// The last row is why NULL is a type here. The sniffer calls a value that was
// only ever null `JSON` — the same word it uses for a real conflict — and
// merging an unknown as a conflict would turn the column to JSON for good the
// first time a file left a field null. So a sniffed JSON leaf is read as
// "unknown" (nullType) and emitted as JSON only if nothing ever says more. The
// cost is that a conflict inside ONE batch, met by a clean type in another,
// keeps the clean type; the batch's odd values then read as NULL (the views
// set ignore_errors, which makes a failed cast NULL, never a lost row).
//
// Field names compare case-insensitively and the first spelling wins outright,
// as in DuckDB: the native broadcaster reports `FramesDroppedAtSend` beside the
// browser's `framesDroppedAtSend`, both land in one `stats` struct, and the
// reader then reads only the first spelling — so the second's type is not
// merged in, because none of its values are ever read.

import (
	"fmt"
	"strings"
)

// colType is one parsed column type. kind is the type's name in upper case,
// with three structured kinds that carry children.
type colType struct {
	kind   string
	fields []colField // structKind
	elem   *colType   // listKind; the value type for mapKind
	key    *colType   // mapKind
}

type colField struct {
	name string
	t    *colType
}

const (
	structKind = "STRUCT"
	listKind   = "LIST"
	mapKind    = "MAP"
	// nullType is "nothing on disk says what this is yet". It never reaches
	// DuckDB: emit writes it as JSON, which holds anything.
	nullType = "NULL"
	jsonType = "JSON"
)

func scalar(kind string) *colType { return &colType{kind: kind} }

// parseType reads a type as DESCRIBE prints it: `BIGINT`, `TIMESTAMP WITH TIME
// ZONE`, `DECIMAL(18,3)`, `STRUCT(a BIGINT, "b c" VARCHAR)`, `MAP(VARCHAR,
// JSON)`, `BIGINT[]`.
func parseType(s string) (*colType, error) {
	p := &typeParser{s: s}
	t, err := p.typ()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("sqlengine: type %q: unexpected %q", s, p.s[p.i:])
	}
	return t, nil
}

type typeParser struct {
	s string
	i int
}

func (p *typeParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *typeParser) peek(c byte) bool {
	p.space()
	return p.i < len(p.s) && p.s[p.i] == c
}

func (p *typeParser) expect(c byte) error {
	if !p.peek(c) {
		return fmt.Errorf("sqlengine: type %q: want %q at %d", p.s, c, p.i)
	}
	p.i++
	return nil
}

// word reads a bare identifier.
func (p *typeParser) word() string {
	p.space()
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

// name reads a struct field name, bare or "double-quoted" with "" escapes.
func (p *typeParser) name() (string, error) {
	if !p.peek('"') {
		if w := p.word(); w != "" {
			return w, nil
		}
		return "", fmt.Errorf("sqlengine: type %q: want a field name at %d", p.s, p.i)
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		if c != '"' {
			b.WriteByte(c)
			continue
		}
		if p.i < len(p.s) && p.s[p.i] == '"' {
			b.WriteByte('"')
			p.i++
			continue
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("sqlengine: type %q: unterminated name", p.s)
}

func (p *typeParser) typ() (*colType, error) {
	head := strings.ToUpper(p.word())
	if head == "" {
		return nil, fmt.Errorf("sqlengine: type %q: want a type at %d", p.s, p.i)
	}
	var t *colType
	switch head {
	case structKind:
		t = &colType{kind: structKind}
		if err := p.expect('('); err != nil {
			return nil, err
		}
		for {
			n, err := p.name()
			if err != nil {
				return nil, err
			}
			ft, err := p.typ()
			if err != nil {
				return nil, err
			}
			t.fields = append(t.fields, colField{n, ft})
			if !p.peek(',') {
				break
			}
			p.i++
		}
		if err := p.expect(')'); err != nil {
			return nil, err
		}
	case mapKind:
		if err := p.expect('('); err != nil {
			return nil, err
		}
		k, err := p.typ()
		if err != nil {
			return nil, err
		}
		if err := p.expect(','); err != nil {
			return nil, err
		}
		v, err := p.typ()
		if err != nil {
			return nil, err
		}
		if err := p.expect(')'); err != nil {
			return nil, err
		}
		t = &colType{kind: mapKind, key: k, elem: v}
	default:
		// Multi-word names (TIMESTAMP WITH TIME ZONE) and parameters
		// (DECIMAL(18,3)) are kept verbatim: merge only ever compares them.
		for {
			save := p.i
			w := p.word()
			if w == "" || strings.EqualFold(w, "STRUCT") || strings.EqualFold(w, "MAP") {
				p.i = save
				break
			}
			head += " " + strings.ToUpper(w)
		}
		if p.peek('(') {
			start := p.i
			depth := 0
			for p.i < len(p.s) {
				c := p.s[p.i]
				p.i++
				if c == '(' {
					depth++
				} else if c == ')' {
					if depth--; depth == 0 {
						break
					}
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("sqlengine: type %q: unbalanced parentheses", p.s)
			}
			head += strings.ReplaceAll(p.s[start:p.i], " ", "")
		}
		t = scalar(head)
	}
	for p.peek('[') {
		p.i++
		if err := p.expect(']'); err != nil {
			return nil, err
		}
		t = &colType{kind: listKind, elem: t}
	}
	return t, nil
}

// String writes the type in DuckDB's syntax with unknowns kept as NULL — the
// learned form, which a later batch can still narrow.
func (t *colType) String() string { return t.write(nullType) }

// emit writes the type for DuckDB to read with, unknowns as JSON.
func (t *colType) emit() string { return t.write(jsonType) }

func (t *colType) write(unknown string) string {
	switch t.kind {
	case structKind:
		parts := make([]string, len(t.fields))
		for i, f := range t.fields {
			parts[i] = quoteIdent(f.name) + " " + f.t.write(unknown)
		}
		return "STRUCT(" + strings.Join(parts, ", ") + ")"
	case listKind:
		return t.elem.write(unknown) + "[]"
	case mapKind:
		return "MAP(" + t.key.write(unknown) + ", " + t.elem.write(unknown) + ")"
	case nullType:
		return unknown
	}
	return t.kind
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// sniffed reads a type as the sniffer reported it: its JSON leaves are
// unknowns, not conflicts (see the file comment).
func sniffed(t *colType) *colType {
	switch t.kind {
	case jsonType:
		return scalar(nullType)
	case structKind:
		out := &colType{kind: structKind, fields: make([]colField, len(t.fields))}
		for i, f := range t.fields {
			out.fields[i] = colField{f.name, sniffed(f.t)}
		}
		return out
	case listKind:
		return &colType{kind: listKind, elem: sniffed(t.elem)}
	case mapKind:
		return &colType{kind: mapKind, key: t.key, elem: sniffed(t.elem)}
	}
	return t
}

// numeric are the sniffer's number types; two different ones merge to DOUBLE.
var numeric = map[string]bool{
	"TINYINT": true, "SMALLINT": true, "INTEGER": true, "BIGINT": true, "HUGEINT": true,
	"UTINYINT": true, "USMALLINT": true, "UINTEGER": true, "UBIGINT": true, "UHUGEINT": true,
	"FLOAT": true, "DOUBLE": true,
}

// stringish are the types the sniffer makes out of JSON strings; two
// different ones merge to VARCHAR, which every one of them was on disk.
var stringish = map[string]bool{
	"VARCHAR": true, "DATE": true, "TIME": true, "TIMESTAMP": true,
	"TIMESTAMP WITH TIME ZONE": true, "UUID": true,
}

// merge is the type that holds what both a and b hold. It never modifies
// either.
func merge(a, b *colType) *colType {
	switch {
	case a.kind == nullType:
		return b
	case b.kind == nullType:
		return a
	case a.kind == mapKind && b.kind == structKind && a.elem.kind == nullType:
		// `{}` sniffs as MAP(VARCHAR, JSON): an object with no keys says
		// nothing against one with some.
		return b
	case b.kind == mapKind && a.kind == structKind && b.elem.kind == nullType:
		return a
	case a.kind != b.kind:
		switch {
		case numeric[a.kind] && numeric[b.kind]:
			return scalar("DOUBLE")
		case stringish[a.kind] && stringish[b.kind]:
			return scalar("VARCHAR")
		}
		return scalar(jsonType)
	}
	switch a.kind {
	case structKind:
		return mergeFields(a, b)
	case listKind:
		return &colType{kind: listKind, elem: merge(a.elem, b.elem)}
	case mapKind:
		return &colType{kind: mapKind, key: merge(a.key, b.key), elem: merge(a.elem, b.elem)}
	}
	return a
}

// mergeFields is a's fields in a's order, each merged with b's field of the
// same name, then b's new fields in b's order. A b field that differs from an
// a field only in case is dropped (see the file comment).
func mergeFields(a, b *colType) *colType {
	out := &colType{kind: structKind, fields: make([]colField, 0, len(a.fields))}
	idx := make(map[string]int, len(a.fields))
	for _, f := range a.fields {
		idx[strings.ToLower(f.name)] = len(out.fields)
		out.fields = append(out.fields, f)
	}
	for _, f := range b.fields {
		if i, ok := idx[strings.ToLower(f.name)]; ok {
			if out.fields[i].name == f.name {
				out.fields[i].t = merge(out.fields[i].t, f.t)
			}
			continue
		}
		idx[strings.ToLower(f.name)] = len(out.fields)
		out.fields = append(out.fields, f)
	}
	return out
}
