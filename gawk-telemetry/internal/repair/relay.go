package repair

import (
	"bytes"
	"encoding/binary"
	"hash/maphash"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The relay pass is the expensive half: production's relay files are
// ~800 MB of gzip, several GB of JSON, read on a pod with a 500m CPU request
// while ingest answers 503. A first version decoded every line with
// encoding/json and re-encoded marked lines through maps; at production size
// that took ~2.5 min per date and over 1.4 GiB of RSS. So this pass never
// builds a JSON value:
//
//   - pass 1 walks each line with a small scanner (scan.go) that extracts
//     only atMs, broadcastKey, sessionId and each subscriber object's
//     internal / stripeLeg / keyframe counters, plus WHERE that object ends;
//   - groups live in a map keyed by a 128-bit hash of the group identity,
//     with members in one flat arena, so the per-date index holds no
//     pointers and no strings for the garbage collector to walk;
//   - pass 2 streams only the files holding a duplicate and applies byte
//     edits — insert `"stripeLeg":true` before the object's closing brace,
//     or overwrite an existing stripeLeg value — so every other byte, key
//     order included, is the one that was stored.
//
// A group is every audience-looking subscriber entry of one session in one
// scrape round. A round is one ScrapeOnce pass: every pod's observations
// carry the SAME atMs (relayscrape stamps them from one clock reading), and a
// leg may land on any pod behind the UDP LB, so a group spans all pod files of
// the date. Stored subscriber lines and the broadcast lines'
// subscriberDetails are grouped separately but judged by the same rule, so
// they agree.
//
// Grouping across pods needs a fleet-wide statsKey (GAWK_STATS_KEY): the
// broadcastKey is the relay's HMAC of the broadcast ID, and a pod without the
// shared key draws its own, so one broadcast's key differs per pod and a leg
// on another pod is never grouped with its primary (docs/33 §4.17).

// groupID is a 128-bit hash of (atMs, broadcastKey, sessionId, detail). Two
// independent 64-bit maphash seeds: ~1M groups per date collide with
// probability around 1e-27, and a collision could only mark a viewer as a
// leg in one round of raw data that expires with the retention window.
type groupID [2]uint64

type group struct {
	n     int32  // audience members
	keep  int32  // arena index of the member that stays
	head  int32  // arena index of the last member added (a linked list)
	score uint64 // the kept member's keyframes
}

// member is one audience subscriber entry and the edit that would mark it.
type member struct {
	next   int32
	file   int32
	line   int32
	pos    int32 // closing brace offset, or the existing stripeLeg value's start
	end    int32 // existing stripeLeg value's end; 0 = insert at pos
	empty  bool  // the object is `{}`: insert without a leading comma
	detail bool
}

type relayIndex struct {
	seeds  [2]maphash.Seed
	buf    []byte
	groups map[groupID]group
	arena  []member
}

func (x *relayIndex) id(atMs int64, bkey, sid []byte, detail bool) groupID {
	x.buf = binary.LittleEndian.AppendUint64(x.buf[:0], uint64(atMs))
	x.buf = append(x.buf, bkey...)
	x.buf = append(x.buf, 0)
	x.buf = append(x.buf, sid...)
	if detail {
		x.buf = append(x.buf, 1)
	}
	return groupID{maphash.Bytes(x.seeds[0], x.buf), maphash.Bytes(x.seeds[1], x.buf)}
}

func (x *relayIndex) add(atMs int64, bkey []byte, s subEntry, file, line int, detail bool) {
	if s.internal || len(s.sessionID) == 0 {
		return
	}
	k := x.id(atMs, bkey, s.sessionID, detail)
	g, seen := x.groups[k]
	if !seen {
		g = group{keep: -1, head: -1}
	}
	defer func() { x.groups[k] = g }()
	// An entry already marked is not a member: post-#657 data marks every
	// leg, which leaves only the primary and nothing to do. It must not veto
	// the group either, because the marker may be this repair's own from a
	// run that stopped between two pod files' rewrites; the unmarked rest
	// still has to be finished. The primary stays the kept member across
	// such a re-run: it is still the unmarked one with the most keyframes,
	// or on a tie still the first unmarked one in file order.
	if s.stripeLeg {
		return
	}
	m := member{next: g.head, file: int32(file), line: int32(line), pos: int32(s.close), empty: s.empty, detail: detail}
	if s.legStart > 0 {
		m.pos, m.end = int32(s.legStart), int32(s.legEnd)
	}
	x.arena = append(x.arena, m)
	idx := int32(len(x.arena) - 1)
	g.head = idx
	// The primary is the member that received keyframes: a leg never does
	// (docs/35 §5.1). On a tie the first in file order stays.
	if sc := s.keyframes; g.n == 0 || sc > g.score {
		g.score, g.keep = sc, idx
	}
	g.n++
}

func repairRelay(dir string) (RelaySummary, error) {
	var sum RelaySummary
	dates, err := readDir(dir)
	if err != nil {
		return sum, err
	}
	x := &relayIndex{seeds: [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}}
	for _, d := range dates {
		if !d.IsDir() || !datePartRe.MatchString(d.Name()) {
			continue
		}
		sum.DatesScanned++
		if err := repairRelayDate(x, filepath.Join(dir, d.Name()), &sum); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

func repairRelayDate(x *relayIndex, dir string, sum *RelaySummary) error {
	entries, err := readDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)

	// Pass 1: index. The map and arena are per date; a fresh map each date
	// lets the previous one be collected whole.
	x.groups = make(map[groupID]group, 1<<16)
	x.arena = x.arena[:0]
	skipped := map[int]bool{}
	for fi, path := range files {
		err := eachLine(path, func(li int, body []byte) error {
			o, ok := scanObservation(body)
			if !ok {
				if len(bytes.TrimSpace(body)) > 0 {
					sum.UnparsedLinesKept++
				}
				return nil
			}
			if o.hasSub {
				x.add(o.atMs, o.broadcastKey, o.sub, fi, li, false)
			}
			for _, d := range o.details {
				x.add(o.atMs, o.broadcastKey, d, fi, li, true)
			}
			return nil
		})
		if err != nil {
			// A file no reader can read whole either (a gzip cut short by a
			// crash): never rewritten, so its tail is not lost. Entries it
			// held may be indexed; one left unmarked only leaves that round
			// over-counted, as it was.
			skipped[fi] = true
			sum.UnreadableFilesSkipped++
		}
	}

	// Edits, per file, for every non-kept member of a duplicate group.
	edits := map[int][]member{}
	for _, g := range x.groups {
		if g.n < 2 {
			continue
		}
		for i := g.head; i >= 0; i = x.arena[i].next {
			if i == g.keep || skipped[int(x.arena[i].file)] {
				continue
			}
			m := x.arena[i]
			edits[int(m.file)] = append(edits[int(m.file)], m)
		}
	}
	x.groups = nil

	// Pass 2: stream each touched file through its edits.
	for fi, path := range files {
		es := edits[fi]
		if len(es) == 0 {
			continue
		}
		// By line, and within a line from the END backwards, so applying one
		// edit never moves the offset of the next.
		sort.Slice(es, func(i, j int) bool {
			if es[i].line != es[j].line {
				return es[i].line < es[j].line
			}
			return es[i].pos > es[j].pos
		})
		gz := strings.HasSuffix(path, ".gz")
		err := atomicWrite(path, gz, func(w io.Writer) error {
			bw := newLineWriter(w)
			next := 0
			err := eachLineRaw(path, func(li int, ln line) error {
				body := ln.body
				for next < len(es) && int(es[next].line) == li {
					body = applyEdit(body, es[next])
					if es[next].detail {
						sum.SubscriberDetailsMarked++
					} else {
						sum.SubscriberLinesMarked++
					}
					next++
				}
				return bw.write(line{body: body, nl: ln.nl})
			})
			if err != nil {
				return err
			}
			return bw.flush()
		})
		if err != nil {
			return err
		}
		sum.FilesRewritten++
	}
	return nil
}

func applyEdit(body []byte, m member) []byte {
	var ins string
	switch {
	case m.end > 0:
		// An existing stripeLeg key (false, or any non-true value): overwrite.
		out := make([]byte, 0, len(body)+4)
		out = append(out, body[:m.pos]...)
		out = append(out, "true"...)
		return append(out, body[m.end:]...)
	case m.empty:
		ins = `"stripeLeg":true`
	default:
		ins = `,"stripeLeg":true`
	}
	out := make([]byte, 0, len(body)+len(ins))
	out = append(out, body[:m.pos]...)
	out = append(out, ins...)
	return append(out, body[m.pos:]...)
}

// --- the scanner -------------------------------------------------------------

// subEntry is what pass 1 needs from one subscriber object.
type subEntry struct {
	sessionID        []byte
	internal         bool
	stripeLeg        bool
	keyframes        uint64
	close            int // offset of the object's closing brace
	empty            bool
	legStart, legEnd int // span of an existing stripeLeg value, if any
}

type observationScan struct {
	atMs         int64
	broadcastKey []byte
	sessionID    []byte
	hasSub       bool
	sub          subEntry
	details      []subEntry
}

// scanObservation extracts the fields the relay pass needs from one stored
// line. ok is false for anything that is not a JSON object the scanner can
// walk; such a line is kept verbatim and never edited.
func scanObservation(b []byte) (o observationScan, ok bool) {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return o, false
	}
	var subStart, bcStart int = -1, -1
	_, ok = walkObject(b, i, func(key []byte, vs, ve int) bool {
		switch string(key) {
		case "atMs":
			n, err := strconv.ParseInt(string(b[vs:ve]), 10, 64)
			if err != nil {
				return false
			}
			o.atMs = n
		case "broadcastKey":
			o.broadcastKey = stringBytes(b, vs, ve)
		case "sessionId":
			o.sessionID = stringBytes(b, vs, ve)
		case "subscriber":
			if b[vs] == '{' {
				subStart = vs
			}
		case "broadcast":
			if b[vs] == '{' {
				bcStart = vs
			}
		}
		return true
	})
	if !ok {
		return o, false
	}
	if subStart >= 0 {
		s, ok := scanSubscriber(b, subStart)
		if !ok {
			return o, false
		}
		// Stored lines join on the TOP-LEVEL sessionId (what JoinRelay
		// matches), not the nested copy.
		s.sessionID = o.sessionID
		o.sub, o.hasSub = s, true
	}
	if bcStart >= 0 {
		ok := true
		walkObject(b, bcStart, func(key []byte, vs, ve int) bool {
			if string(key) != "subscriberDetails" || b[vs] != '[' {
				return true
			}
			j := skipWS(b, vs+1)
			for j < ve-1 && ok {
				if b[j] == '{' {
					s, sok := scanSubscriber(b, j)
					if !sok {
						ok = false
						break
					}
					o.details = append(o.details, s)
				}
				end, vok := skipValue(b, j)
				if !vok {
					ok = false
					break
				}
				j = skipWS(b, end)
				if j < len(b) && b[j] == ',' {
					j = skipWS(b, j+1)
				}
			}
			return true
		})
		if !ok {
			return o, false
		}
	}
	return o, true
}

func scanSubscriber(b []byte, start int) (s subEntry, ok bool) {
	fields := 0
	var kfSent, kfDropped uint64
	end, ok := walkObject(b, start, func(key []byte, vs, ve int) bool {
		fields++
		switch string(key) {
		case "sessionId":
			s.sessionID = stringBytes(b, vs, ve)
		case "internal":
			s.internal = string(b[vs:ve]) == "true"
		case "stripeLeg":
			s.stripeLeg = string(b[vs:ve]) == "true"
			s.legStart, s.legEnd = vs, ve
		case "keyframesSent":
			kfSent, _ = strconv.ParseUint(string(b[vs:ve]), 10, 64)
		case "keyframesDropped":
			kfDropped, _ = strconv.ParseUint(string(b[vs:ve]), 10, 64)
		}
		return true
	})
	s.close, s.empty, s.keyframes = end, fields == 0, kfSent+kfDropped
	return s, ok
}

// stringBytes returns a JSON string value's raw bytes without its quotes, or
// nil for a non-string. Escapes are left as written: the ids this keys on
// are hex, and two spellings of one id would at worst stay ungrouped.
func stringBytes(b []byte, vs, ve int) []byte {
	if ve-vs < 2 || b[vs] != '"' {
		return nil
	}
	return b[vs+1 : ve-1]
}
