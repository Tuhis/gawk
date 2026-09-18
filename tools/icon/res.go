package main

// The compiled Windows resource file (.res) that carries the icon into
// gawk-broadcast.exe (docs/53 D7). lld-link and link.exe take a .res as an
// input file directly, so generating it here is what makes a resource
// compiler unnecessary in the cross-compile.
//
// Format (the same one rc.exe emits): a 32-byte empty header resource, then
// one record per resource — DataSize, HeaderSize, Type, Name, DataVersion,
// MemoryFlags, LanguageId, Version, Characteristics, then the data, padded
// to a 4-byte boundary. Type and Name are ordinals here (0xFFFF marker + a
// 16-bit ID), so every header is exactly 32 bytes.
//
// The icon is RT_ICON records 1..n (one payload each, identical to the .ico
// entries) plus one RT_GROUP_ICON whose directory is the .ico directory with
// 14-byte entries naming resource IDs instead of file offsets.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
)

const (
	rtIcon      = 3
	rtGroupIcon = 14
	resHeader   = 32
	// The group icon's resource ID. Explorer uses the first group in resource
	// order, so the number is not load-bearing; 1 is what rc.exe would give a
	// single ICON statement.
	groupIconID = 1
	langNeutral = 0
	// MOVEABLE|DISCARDABLE, what rc.exe marks icons with. Linkers ignore it.
	memIcon = 0x1010
)

// Resource is one decoded .res record with ordinal type and name.
type Resource struct {
	Type, ID uint16
	Data     []byte
}

func writeResRecord(buf *bytes.Buffer, typ, id uint16, data []byte) {
	var h [resHeader]byte
	binary.LittleEndian.PutUint32(h[0:], uint32(len(data)))
	binary.LittleEndian.PutUint32(h[4:], resHeader)
	binary.LittleEndian.PutUint16(h[8:], 0xFFFF)
	binary.LittleEndian.PutUint16(h[10:], typ)
	binary.LittleEndian.PutUint16(h[12:], 0xFFFF)
	binary.LittleEndian.PutUint16(h[14:], id)
	// DataVersion (16..20) = 0
	binary.LittleEndian.PutUint16(h[20:], memIcon)
	binary.LittleEndian.PutUint16(h[22:], langNeutral)
	// Version (24..28) = 0, Characteristics (28..32) = 0
	buf.Write(h[:])
	buf.Write(data)
	if pad := (4 - len(data)%4) % 4; pad != 0 {
		buf.Write(make([]byte, pad))
	}
}

// WriteRES builds the resource file for the given images.
func WriteRES(images map[int]*image.NRGBA) ([]byte, error) {
	sizes := sortedSizes(images)
	if len(sizes) == 0 {
		return nil, fmt.Errorf("res: no images")
	}
	var buf bytes.Buffer
	// The empty header resource every .res starts with: DataSize 0,
	// HeaderSize 32, type 0, name 0, everything else 0.
	var lead [resHeader]byte
	binary.LittleEndian.PutUint32(lead[4:], resHeader)
	binary.LittleEndian.PutUint16(lead[8:], 0xFFFF)
	binary.LittleEndian.PutUint16(lead[12:], 0xFFFF)
	buf.Write(lead[:])

	var dir bytes.Buffer
	dirHeader := []byte{0, 0, 1, 0, 0, 0}
	binary.LittleEndian.PutUint16(dirHeader[4:], uint16(len(sizes)))
	dir.Write(dirHeader)
	for i, s := range sizes {
		e, data, err := encodeEntry(images[s])
		if err != nil {
			return nil, err
		}
		id := uint16(i + 1)
		writeResRecord(&buf, rtIcon, id, data)
		var ge [14]byte
		ge[0], ge[1], ge[2], ge[3] = e.Width, e.Height, e.Colors, e.Reserved
		binary.LittleEndian.PutUint16(ge[4:], e.Planes)
		binary.LittleEndian.PutUint16(ge[6:], e.BitCount)
		binary.LittleEndian.PutUint32(ge[8:], e.Size)
		binary.LittleEndian.PutUint16(ge[12:], id)
		dir.Write(ge[:])
	}
	writeResRecord(&buf, rtGroupIcon, groupIconID, dir.Bytes())
	return buf.Bytes(), nil
}

// ParseRES reads every record with ordinal type and name. String-named
// records are rejected: this writer never produces them, so one is drift.
func ParseRES(data []byte) ([]Resource, error) {
	var out []Resource
	p := 0
	for p < len(data) {
		if len(data)-p < resHeader {
			return nil, fmt.Errorf("res: truncated header at %d", p)
		}
		dataSize := int(binary.LittleEndian.Uint32(data[p:]))
		headerSize := int(binary.LittleEndian.Uint32(data[p+4:]))
		if headerSize != resHeader {
			return nil, fmt.Errorf("res: record at %d has header size %d (string-named?), want %d", p, headerSize, resHeader)
		}
		if binary.LittleEndian.Uint16(data[p+8:]) != 0xFFFF || binary.LittleEndian.Uint16(data[p+12:]) != 0xFFFF {
			return nil, fmt.Errorf("res: record at %d is not ordinal-typed and -named", p)
		}
		typ := binary.LittleEndian.Uint16(data[p+10:])
		id := binary.LittleEndian.Uint16(data[p+14:])
		start := p + headerSize
		if start+dataSize > len(data) {
			return nil, fmt.Errorf("res: record at %d runs past the end", p)
		}
		if !(p == 0 && dataSize == 0 && typ == 0 && id == 0) {
			out = append(out, Resource{Type: typ, ID: id, Data: data[start : start+dataSize]})
		} else if p != 0 {
			return nil, fmt.Errorf("res: empty record at %d", p)
		}
		p = start + dataSize
		p += (4 - p%4) % 4
	}
	return out, nil
}

// GroupEntry is one RT_GROUP_ICON directory entry.
type GroupEntry struct {
	Entry icoEntry
	ID    uint16
}

// ParseGroupIcon decodes an RT_GROUP_ICON payload.
func ParseGroupIcon(data []byte) ([]GroupEntry, error) {
	if len(data) < 6 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 {
		return nil, fmt.Errorf("res: group icon: bad directory header")
	}
	n := int(binary.LittleEndian.Uint16(data[4:]))
	if len(data) != 6+14*n {
		return nil, fmt.Errorf("res: group icon: %d bytes for %d entries", len(data), n)
	}
	out := make([]GroupEntry, 0, n)
	for i := 0; i < n; i++ {
		b := data[6+14*i:]
		out = append(out, GroupEntry{
			Entry: icoEntry{
				Width: b[0], Height: b[1], Colors: b[2], Reserved: b[3],
				Planes:   binary.LittleEndian.Uint16(b[4:]),
				BitCount: binary.LittleEndian.Uint16(b[6:]),
				Size:     binary.LittleEndian.Uint32(b[8:]),
			},
			ID: binary.LittleEndian.Uint16(b[12:]),
		})
	}
	return out, nil
}

// ResIcons decodes a .res back into size → image, checking that the group
// directory and the icon records agree with each other.
func ResIcons(data []byte) (map[int]*image.NRGBA, error) {
	recs, err := ParseRES(data)
	if err != nil {
		return nil, err
	}
	icons := map[uint16][]byte{}
	var groups [][]GroupEntry
	for _, r := range recs {
		switch r.Type {
		case rtIcon:
			icons[r.ID] = r.Data
		case rtGroupIcon:
			g, err := ParseGroupIcon(r.Data)
			if err != nil {
				return nil, err
			}
			groups = append(groups, g)
		default:
			return nil, fmt.Errorf("res: unexpected resource type %d", r.Type)
		}
	}
	if len(groups) != 1 {
		return nil, fmt.Errorf("res: %d RT_GROUP_ICON records, want 1", len(groups))
	}
	if len(groups[0]) != len(icons) {
		return nil, fmt.Errorf("res: group names %d icons, file holds %d", len(groups[0]), len(icons))
	}
	out := make(map[int]*image.NRGBA, len(icons))
	for _, ge := range groups[0] {
		payload, ok := icons[ge.ID]
		if !ok {
			return nil, fmt.Errorf("res: group names icon %d, which is missing", ge.ID)
		}
		if int(ge.Entry.Size) != len(payload) {
			return nil, fmt.Errorf("res: icon %d: group says %d bytes, record holds %d", ge.ID, ge.Entry.Size, len(payload))
		}
		img, err := decodeEntry(payload)
		if err != nil {
			return nil, fmt.Errorf("res: icon %d: %w", ge.ID, err)
		}
		want := int(ge.Entry.Width)
		if want == 0 {
			want = 256
		}
		if img.Bounds().Dx() != want {
			return nil, fmt.Errorf("res: icon %d: group says %d px, holds %d", ge.ID, want, img.Bounds().Dx())
		}
		if _, dup := out[want]; dup {
			return nil, fmt.Errorf("res: two icons of %d px", want)
		}
		out[want] = img
	}
	return out, nil
}
