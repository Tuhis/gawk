package main

import (
	"encoding/binary"
	"testing"
)

func TestRESStartsWithTheEmptyHeaderResource(t *testing.T) {
	data, err := WriteRES(testImages(16))
	if err != nil {
		t.Fatal(err)
	}
	// DataSize 0, HeaderSize 32, type 0xFFFF/0, name 0xFFFF/0.
	want := []byte{0, 0, 0, 0, 32, 0, 0, 0, 0xff, 0xff, 0, 0, 0xff, 0xff, 0, 0}
	for i, b := range want {
		if data[i] != b {
			t.Fatalf("byte %d = %#x, want %#x", i, data[i], b)
		}
	}
	for i := 16; i < 32; i++ {
		if data[i] != 0 {
			t.Fatalf("byte %d of the lead resource = %#x, want 0", i, data[i])
		}
	}
}

func TestRESHoldsOneIconPerSizeAndOneGroupNamingThem(t *testing.T) {
	images := testImages(Sizes...)
	data, err := WriteRES(images)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%4 != 0 {
		t.Fatalf("length %d is not 4-aligned", len(data))
	}
	recs, err := ParseRES(data)
	if err != nil {
		t.Fatal(err)
	}
	var icons, groups int
	for _, r := range recs {
		switch r.Type {
		case rtIcon:
			icons++
			if r.ID != uint16(icons) {
				t.Fatalf("icon IDs are not 1..n in order: got %d at position %d", r.ID, icons)
			}
		case rtGroupIcon:
			groups++
			if r.ID != groupIconID {
				t.Fatalf("group ID %d", r.ID)
			}
			entries, err := ParseGroupIcon(r.Data)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(Sizes) {
				t.Fatalf("group has %d entries", len(entries))
			}
			for i, s := range Sizes {
				if entries[i].ID != uint16(i+1) {
					t.Fatalf("group entry %d names icon %d", i, entries[i].ID)
				}
				if int(entries[i].Entry.Width) != s%256 {
					t.Fatalf("group entry %d is %d px, want %d", i, entries[i].Entry.Width, s%256)
				}
			}
		default:
			t.Fatalf("unexpected resource type %d", r.Type)
		}
	}
	if icons != len(Sizes) || groups != 1 {
		t.Fatalf("%d icons, %d groups", icons, groups)
	}
	// Language and memory flags on the first real record, as rc.exe writes.
	first := data[resHeader:]
	if binary.LittleEndian.Uint16(first[20:]) != memIcon || binary.LittleEndian.Uint16(first[22:]) != langNeutral {
		t.Fatalf("memory flags / language: %x", first[20:24])
	}

	decoded, err := ResIcons(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Sizes {
		sameImage(t, decoded[s], images[s])
	}
}

func TestResIconsRejectsAGroupThatDisagreesWithItsIcons(t *testing.T) {
	data, err := WriteRES(testImages(16, 32))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ParseRES(data)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the group's second entry to name an icon that does not exist.
	// The group is the last record; find its data in the original bytes.
	group := recs[len(recs)-1]
	if group.Type != rtGroupIcon {
		t.Fatalf("last record type %d", group.Type)
	}
	binary.LittleEndian.PutUint16(group.Data[6+14+12:], 9)
	if _, err := ResIcons(data); err == nil {
		t.Fatal("a group naming a missing icon passed")
	}
}

func TestParseRESRejectsStringNamedRecords(t *testing.T) {
	data, err := WriteRES(testImages(16))
	if err != nil {
		t.Fatal(err)
	}
	// Make the first real record's type a string (no 0xFFFF marker).
	binary.LittleEndian.PutUint16(data[resHeader+8:], 'A')
	if _, err := ParseRES(data); err == nil {
		t.Fatal("string-named record parsed")
	}
}
