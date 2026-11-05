package envspec

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

var idMu sync.Mutex
var lastMS int64
var seq uint16

// NewID returns a prefixed UUIDv7-style ID: a 48-bit Unix-ms timestamp,
// version/variant bits and random rest, hex with dashes. Prefixes say what
// an ID is ("env_", "lease_", "cred_", "col_", "proc_", "cap_", "op_"), so a
// mixed-up argument fails with a clear message.
func NewID(prefix string) string {
	idMu.Lock()
	ms := time.Now().UnixMilli()
	if ms == lastMS {
		seq++
	} else {
		lastMS, seq = ms, 0
	}
	s := seq
	idMu.Unlock()

	var b [16]byte
	_, _ = rand.Read(b[:])
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	// 12-bit sub-ms sequence in the rand_a field keeps IDs sortable.
	b[6] = 0x70 | byte(s>>8)&0x0f
	b[7] = byte(s)
	b[8] = 0x80 | b[8]&0x3f
	h := hex.EncodeToString(b[:])
	return prefix + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// HasPrefix reports whether id carries the given kind prefix.
func HasPrefix(id, prefix string) bool {
	return len(id) > len(prefix) && id[:len(prefix)] == prefix
}
