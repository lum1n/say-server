package core

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// NewID returns a UUIDv7: random, but sortable by creation time.
func NewID() string {
	var b [16]byte
	rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(b[4:6], uint16(ms))
	binary.BigEndian.PutUint32(b[0:4], uint32(ms>>16))
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
