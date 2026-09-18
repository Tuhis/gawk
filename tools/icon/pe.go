package main

// verify-exe: the CI proof that the icon resource reached the linked binary
// (docs/53 D8). Walks the top level of the PE's resource directory and
// reports the resource types present.

import (
	"debug/pe"
	"encoding/binary"
	"fmt"
	"slices"
)

// ResourceTypes returns the ordinal type IDs at the root of an
// IMAGE_RESOURCE_DIRECTORY (the raw .rsrc section contents). Named types are
// skipped: icons are always ordinal.
func ResourceTypes(rsrc []byte) ([]uint32, error) {
	const dirHeader = 16
	if len(rsrc) < dirHeader {
		return nil, fmt.Errorf("rsrc: %d bytes is shorter than a directory", len(rsrc))
	}
	named := int(binary.LittleEndian.Uint16(rsrc[12:]))
	ids := int(binary.LittleEndian.Uint16(rsrc[14:]))
	if len(rsrc) < dirHeader+8*(named+ids) {
		return nil, fmt.Errorf("rsrc: directory claims %d entries but is truncated", named+ids)
	}
	out := make([]uint32, 0, ids)
	for i := named; i < named+ids; i++ {
		e := rsrc[dirHeader+8*i:]
		out = append(out, binary.LittleEndian.Uint32(e)&0x7FFFFFFF)
	}
	return out, nil
}

// VerifyEXE fails unless the executable carries both an RT_GROUP_ICON and
// an RT_ICON — the two halves Explorer needs.
func VerifyEXE(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	s := f.Section(".rsrc")
	if s == nil {
		return fmt.Errorf("%s: no .rsrc section — no resource was linked in", path)
	}
	data, err := s.Data()
	if err != nil {
		return fmt.Errorf("%s: .rsrc: %w", path, err)
	}
	types, err := ResourceTypes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, want := range []struct {
		id   uint32
		name string
	}{{rtGroupIcon, "RT_GROUP_ICON"}, {rtIcon, "RT_ICON"}} {
		if !slices.Contains(types, want.id) {
			return fmt.Errorf("%s: no %s resource (types present: %v)", path, want.name, types)
		}
	}
	return nil
}
