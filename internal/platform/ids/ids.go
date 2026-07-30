// Package ids generates ULID-style sortable identifiers (§1.1 event ids).
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu     sync.Mutex
	lastMs int64
	seq    uint16
)

// New returns a 26-char ULID-compatible identifier: 48-bit millisecond
// timestamp + 16-bit per-ms sequence + 64-bit randomness, Crockford base32.
func New() string {
	ms := time.Now().UnixMilli()
	mu.Lock()
	if ms == lastMs {
		seq++
	} else {
		lastMs = ms
		seq = 0
	}
	s := seq
	mu.Unlock()

	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(ms)<<16|uint64(s))
	if _, err := rand.Read(b[8:]); err != nil {
		panic(err)
	}
	return encode(b[:])
}

// WithPrefix returns e.g. "op_01J..." style identifiers.
func WithPrefix(prefix string) string { return prefix + "_" + New() }

// encode Crockford-base32 encodes 16 bytes into 26 chars (128 bits -> ceil(128/5)=26).
func encode(data []byte) string {
	out := make([]byte, 26)
	var acc uint32
	var bits uint
	di := 0
	oi := 0
	for oi < 26 {
		for bits < 5 && di < len(data) {
			acc = acc<<8 | uint32(data[di])
			di++
			bits += 8
		}
		if bits < 5 {
			acc <<= 5 - bits
			bits = 5
		}
		bits -= 5
		out[oi] = crockford[(acc>>bits)&31]
		oi++
	}
	return string(out)
}
