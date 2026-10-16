package checkout

// ULID — Universally Unique Lexicographically Sortable Identifier (https://github.com/ulid/spec)
//
//	01JAB3K7Z9Q4M2X8V5N6P0R1ST
//	└─time (10)─┘└─random (16)─┘   26 chars, Crockford base32, sorts by creation time.
//
// Used for user ids, organisation ids and order ids. Users and organisations come
// from the 129Knots platform (already ULIDs); order ids are minted here.

import (
	"crypto/rand"
	"regexp"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // no I, L, O, U

var ulidRe = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// NewULID returns a new ULID for the current time.
func NewULID() string { return newULIDAt(time.Now()) }

func newULIDAt(t time.Time) string {
	var b [26]byte
	ms := uint64(t.UnixMilli())
	for i := 9; i >= 0; i-- {
		b[i] = crockford[ms%32]
		ms /= 32
	}
	var r [16]byte
	_, _ = rand.Read(r[:])
	for i := 0; i < 16; i++ {
		b[10+i] = crockford[r[i]%32]
	}
	return string(b[:])
}

// IsULID reports whether s is a well-formed ULID.
func IsULID(s string) bool { return ulidRe.MatchString(s) }
