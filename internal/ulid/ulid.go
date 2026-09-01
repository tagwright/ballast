// SPDX-License-Identifier: GPL-3.0-or-later

// Package ulid generates ULIDs: 128-bit identifiers rendered as 26 uppercase
// Crockford base32 characters, sortable by their leading 48-bit millisecond
// timestamp and unique by their trailing 80 CSPRNG bits.
//
// Ballast uses them for run_id (and every other id the record contracts call
// for). The format is frozen by those contracts as
// ^[0-9A-HJKMNP-TV-Z]{26}$, which is exactly the Crockford alphabet below,
// uppercase, with I, L, O, and U excluded.
package ulid

import (
	"crypto/rand"
	"fmt"
	"time"
)

// crockford is the Crockford base32 alphabet: the digits and uppercase
// letters with I, L, O, and U removed. Encoding is most-significant-bit
// first.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a fresh ULID: a 48-bit big-endian millisecond timestamp
// followed by 80 CSPRNG bits, encoded as 26 Crockford base32 characters.
func New() (string, error) {
	var b [16]byte

	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("ulid: read CSPRNG: %w", err)
	}

	return encode(b), nil
}

// encode renders the 16 bytes as 26 Crockford base32 characters. 128 bits
// pad up to 130 (26 * 5) with two leading zero bits, so the first character
// is always in the range 0 to 7 and the whole string matches the frozen
// pattern.
func encode(b [16]byte) string {
	out := make([]byte, 26)

	var acc uint32
	// Seed the accumulator with the two zero pad bits so the first emitted
	// character carries them as its high bits.
	nbits := uint(2)
	idx := 0

	for _, by := range b {
		acc = acc<<8 | uint32(by)
		nbits += 8
		for nbits >= 5 {
			nbits -= 5
			out[idx] = crockford[(acc>>nbits)&0x1f]
			idx++
		}
	}
	return string(out)
}
