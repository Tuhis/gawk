package broadcastid

import (
	"crypto/rand"
	"errors"
	"strings"
)

// Alphabet contains the 31 allowed symbols for broadcast IDs (no 0, O, 1, I, L).
const Alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// Length is the required length of a broadcast ID.
const Length = 6

// ErrInvalidID is returned when an ID fails normalization or validation.
var ErrInvalidID = errors.New("broadcastid: invalid ID")

// Mint generates a new 6-character broadcast ID using crypto/rand.
// The selection is modulo-safe and uniform.
func Mint() (string, error) {
	var id strings.Builder
	id.Grow(Length)
	var buf [12]byte
	for id.Len() < Length {
		_, err := rand.Read(buf[:])
		if err != nil {
			return "", err
		}
		for _, val := range buf {
			if val < 248 { // 31 * 8 = 248, rejection sampling for uniform distribution
				id.WriteByte(Alphabet[val%31])
				if id.Len() == Length {
					break
				}
			}
		}
	}
	return id.String(), nil
}

// Normalize uppercases the input string and validates it against the Alphabet and Length.
func Normalize(s string) (string, error) {
	s = strings.ToUpper(s)
	if len(s) != Length {
		return "", ErrInvalidID
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(Alphabet, s[i]) == -1 {
			return "", ErrInvalidID
		}
	}
	return s, nil
}
