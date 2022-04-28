package slog

import "strings"

type LineBuffer struct {
	buf [][]byte
	pos int
	num int
}

func NewLineBuffer(size int) *LineBuffer {
	return &LineBuffer{
		buf: make([][]byte, size),
		pos: 0,
		num: 0,
	}
}

func (b *LineBuffer) Append(line []byte) {
	b.buf[b.pos] = line
	if b.num < len(b.buf) {
		b.num++
	}
	b.pos = (b.pos + 1) % len(b.buf)
}

func (b *LineBuffer) Read() string {
	buf := &strings.Builder{}
	if b.num < len(b.buf) {
		for i := 0; i < b.num; i++ {
			buf.Write(b.buf[i])
			buf.WriteRune('\n')
		}
		return buf.String()
	}
	for i := b.pos; i < len(b.buf); i++ {
		buf.Write(b.buf[i])
		buf.WriteRune('\n')
	}
	for i := 0; i < b.pos; i++ {
		buf.Write(b.buf[i])
		buf.WriteRune('\n')
	}
	return buf.String()
}
