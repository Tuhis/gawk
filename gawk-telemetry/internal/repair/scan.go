package repair

import (
	"bufio"
	"io"
)

// A minimal JSON walker for the relay pass: enough to find object keys and
// the byte span of each value, without building values or validating
// anything it does not need to step over. Every stored relay line is
// json.Marshal output, and any line this cannot walk is reported not-ok and
// kept untouched, so its leniency can never produce an edit on malformed
// input — only a missed one.

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the offset just past the string starting at b[i] == '"'.
func skipString(b []byte, i int) (int, bool) {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// skipValue returns the offset just past the value starting at b[i].
func skipValue(b []byte, i int) (int, bool) {
	if i >= len(b) {
		return 0, false
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, ok := skipString(b, j)
				if !ok {
					return 0, false
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	default: // number, true, false, null
		j := i
		for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ' ' && b[j] != '\n' && b[j] != '\t' && b[j] != '\r' {
			j++
		}
		return j, j > i
	}
}

// walkObject calls fn for each member of the object at b[i] == '{' with the
// raw key bytes (no quotes) and the value's span [vs, ve). fn returning
// false stops the walk as not-ok. It returns the offset of the closing '}'.
func walkObject(b []byte, i int, fn func(key []byte, vs, ve int) bool) (int, bool) {
	if i >= len(b) || b[i] != '{' {
		return 0, false
	}
	j := skipWS(b, i+1)
	if j < len(b) && b[j] == '}' {
		return j, true
	}
	for j < len(b) {
		if b[j] != '"' {
			return 0, false
		}
		kEnd, ok := skipString(b, j)
		if !ok {
			return 0, false
		}
		key := b[j+1 : kEnd-1]
		j = skipWS(b, kEnd)
		if j >= len(b) || b[j] != ':' {
			return 0, false
		}
		vs := skipWS(b, j+1)
		ve, ok := skipValue(b, vs)
		if !ok || !fn(key, vs, ve) {
			return 0, false
		}
		j = skipWS(b, ve)
		if j >= len(b) {
			return 0, false
		}
		switch b[j] {
		case ',':
			j = skipWS(b, j+1)
		case '}':
			return j, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// lineWriter streams lines back out exactly as they were stored.
type lineWriter struct{ bw *bufio.Writer }

func newLineWriter(w io.Writer) *lineWriter { return &lineWriter{bufio.NewWriterSize(w, 256<<10)} }

func (l *lineWriter) write(ln line) error {
	if _, err := l.bw.Write(ln.body); err != nil {
		return err
	}
	if ln.nl {
		return l.bw.WriteByte('\n')
	}
	return nil
}

func (l *lineWriter) flush() error { return l.bw.Flush() }
