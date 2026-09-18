package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// resourceDirectory builds a one-level IMAGE_RESOURCE_DIRECTORY with the
// given ordinal type IDs (plus one named entry, which the walker must skip).
func resourceDirectory(ids ...uint32) []byte {
	b := make([]byte, 16+8*(len(ids)+1))
	binary.LittleEndian.PutUint16(b[12:], 1) // one named entry
	binary.LittleEndian.PutUint16(b[14:], uint16(len(ids)))
	binary.LittleEndian.PutUint32(b[16:], 0x80000000|0x40) // name offset
	binary.LittleEndian.PutUint32(b[20:], 0x80000000|0x80) // subdir
	for i, id := range ids {
		e := b[24+8*i:]
		binary.LittleEndian.PutUint32(e, id)
		binary.LittleEndian.PutUint32(e[4:], 0x80000000|uint32(0x100+0x20*i))
	}
	return b
}

func TestResourceTypesListsOrdinalTypesAndSkipsNamedOnes(t *testing.T) {
	types, err := ResourceTypes(resourceDirectory(rtIcon, rtGroupIcon, 16))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(types, []uint32{rtIcon, rtGroupIcon, 16}) {
		t.Fatalf("types %v", types)
	}
	types, err = ResourceTypes(resourceDirectory(16))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(types, rtGroupIcon) {
		t.Fatalf("found a group icon in %v", types)
	}
}

func TestResourceTypesRejectsTruncatedDirectories(t *testing.T) {
	if _, err := ResourceTypes([]byte{1, 2, 3}); err == nil {
		t.Error("short input parsed")
	}
	d := resourceDirectory(rtIcon)
	if _, err := ResourceTypes(d[:20]); err == nil {
		t.Error("truncated entries parsed")
	}
}

func TestVerifyEXERefusesFilesWithoutAnIconResource(t *testing.T) {
	// A file that is not a PE at all.
	notPE := filepath.Join(t.TempDir(), "x.exe")
	if err := os.WriteFile(notPE, []byte("MZ nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEXE(notPE); err == nil {
		t.Error("non-PE passed")
	}
	if err := VerifyEXE(filepath.Join(t.TempDir(), "missing.exe")); err == nil {
		t.Error("missing file passed")
	}
}
