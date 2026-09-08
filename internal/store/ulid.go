package store

import (
	"crypto/rand"
	"sync"
	"time"
)

// crockford is the ULID alphabet: base32 without I, L, O, and U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ulidClock serializes minting so that ids issued within one millisecond
// still sort in issue order.
var ulidClock struct {
	sync.Mutex
	ms      uint64
	entropy [10]byte
}

// newULID mints an identifier in canonical ULID text: 48 bits of
// millisecond time then 80 random bits, encoded as 26 characters. Time
// leads so that ids sort by creation. Within one millisecond the random
// part is incremented rather than redrawn, so ids minted in sequence by
// this process sort in that sequence: the events of one transaction read
// back in the order they were written. Nothing else reads the layout back.
func newULID() string {
	ulidClock.Lock()
	defer ulidClock.Unlock()

	ms := uint64(time.Now().UnixMilli())
	if ms > ulidClock.ms {
		ulidClock.ms = ms
		if _, err := rand.Read(ulidClock.entropy[:]); err != nil {
			panic(err)
		}
		// Clearing the top bit leaves room for 2^79 increments before
		// the random part could overflow into the timestamp.
		ulidClock.entropy[0] &= 0x7f
	} else {
		ms = ulidClock.ms
		for i := len(ulidClock.entropy) - 1; i >= 0; i-- {
			ulidClock.entropy[i]++
			if ulidClock.entropy[i] != 0 {
				break
			}
		}
	}

	var b [16]byte
	t := ms
	for i := 5; i >= 0; i-- {
		b[i] = byte(t)
		t >>= 8
	}
	copy(b[6:], ulidClock.entropy[:])

	// 128 bits packed into 26 five-bit groups, most significant first. The
	// leading group holds two bits of padding, so it is at most 7.
	var out [26]byte
	var acc uint64
	bits := 0
	pos := len(out) - 1
	for i := len(b) - 1; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	out[0] = crockford[acc&31]
	return string(out[:])
}
