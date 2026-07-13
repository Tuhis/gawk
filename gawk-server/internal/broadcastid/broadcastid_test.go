package broadcastid

import (
	"strings"
	"testing"
)

func TestMint(t *testing.T) {
	// Mint 10,000 samples to verify uniformity and alphabet correctness.
	const samples = 10000
	seen := make(map[rune]bool)
	for i := 0; i < samples; i++ {
		id, err := Mint()
		if err != nil {
			t.Fatalf("Mint failed: %v", err)
		}
		if len(id) != Length {
			t.Errorf("expected ID length %d, got %d for %q", Length, len(id), id)
		}
		for _, r := range id {
			if strings.IndexRune(Alphabet, r) == -1 {
				t.Errorf("character %q not in Alphabet in ID %q", r, id)
			}
			seen[r] = true
		}
	}
	// Verify that every symbol in the alphabet appears at least once.
	for _, r := range Alphabet {
		if !seen[r] {
			t.Errorf("symbol %q never appeared in %d samples", r, samples)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"234567", "234567", false},
		{"23456A", "23456A", false},
		{"23456a", "23456A", false}, // uppercase normalization
		{"2345", "", true},          // too short
		{"2345678", "", true},       // too long
		{"23456O", "", true},        // O is not in alphabet
		{"234560", "", true},        // 0 is not in alphabet
		{"23456I", "", true},        // I is not in alphabet
		{"234561", "", true},        // 1 is not in alphabet
		{"23456L", "", true},        // L is not in alphabet
		{"23456l", "", true},        // l is not in alphabet
		{"234567\x00", "", true},    // null byte / too long
	}

	for _, tt := range tests {
		got, err := Normalize(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("Normalize(%q) error = %v, wantErr = %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
