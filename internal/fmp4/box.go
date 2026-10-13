package fmp4

import (
	"bytes"
	"encoding/binary"
)

// writer is a tiny helper for assembling ISO-BMFF boxes.
type writer struct {
	buf bytes.Buffer
}

func (w *writer) u8(v byte)     { w.buf.WriteByte(v) }
func (w *writer) raw(b []byte)  { w.buf.Write(b) }
func (w *writer) str(s string)  { w.buf.WriteString(s) }
func (w *writer) zeros(n int)   { w.buf.Write(make([]byte, n)) }
func (w *writer) bytes() []byte { return w.buf.Bytes() }

func (w *writer) u16(v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	w.buf.Write(b[:])
}

func (w *writer) u32(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.buf.Write(b[:])
}

func (w *writer) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	w.buf.Write(b[:])
}

// box wraps payload in a box header with the given four-character type.
func box(typ string, payload ...[]byte) []byte {
	size := 8
	for _, p := range payload {
		size += len(p)
	}

	w := &writer{}
	w.u32(uint32(size))
	w.str(typ)
	for _, p := range payload {
		w.raw(p)
	}
	return w.bytes()
}

// fullBox wraps payload in a box header followed by a version/flags word.
func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	w := &writer{}
	w.u32(uint32(version)<<24 | (flags & 0x00ffffff))
	for _, p := range payload {
		w.raw(p)
	}
	return box(typ, w.bytes())
}

// identityMatrix is the unity transformation matrix used in mvhd and tkhd.
var identityMatrix = []uint32{
	0x00010000, 0, 0,
	0, 0x00010000, 0,
	0, 0, 0x40000000,
}

func writeMatrix(w *writer) {
	for _, v := range identityMatrix {
		w.u32(v)
	}
}
