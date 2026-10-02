// Package lume is a key-value store built as a log-structured merge tree.
//
// A write goes to a log on disk and to a sorted table in memory, and that
// is all: no file is ever modified in place. When the memory table fills
// up it is written out as an immutable sorted file, and a background
// process keeps merging those files into larger ones, level by level,
// throwing away what was overwritten or deleted.
package lume

import (
	"bufio"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

const (
	opPut byte = 1
	opDel byte = 2
)

// wal is the write-ahead log: every change is appended here before it is
// applied in memory, so that the memory table can be rebuilt after a crash.
//
// Record: crc32(payload) | len(payload) | payload
// Payload: op | len(key) | key | value
type wal struct {
	f    *os.File
	w    *bufio.Writer
	sync bool
	size int64
	buf  []byte
}

func createWAL(path string, sync bool) (*wal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &wal{f: f, w: bufio.NewWriterSize(f, 64<<10), sync: sync}, nil
}

func (l *wal) append(op byte, key string, value []byte) {
	l.buf = l.buf[:0]
	l.buf = append(l.buf, op)
	l.buf = binary.AppendUvarint(l.buf, uint64(len(key)))
	l.buf = append(l.buf, key...)
	l.buf = append(l.buf, value...)
	var head [8]byte
	binary.LittleEndian.PutUint32(head[0:], crc32.ChecksumIEEE(l.buf))
	binary.LittleEndian.PutUint32(head[4:], uint32(len(l.buf)))
	l.w.Write(head[:])
	l.w.Write(l.buf)
	l.size += int64(8 + len(l.buf))
}

// commit hands what was appended to the operating system, and to the disk
// itself when sync is on. Until this returns, a write isn't durable.
func (l *wal) commit() error {
	if err := l.w.Flush(); err != nil {
		return err
	}
	if l.sync {
		return l.f.Sync()
	}
	return nil
}

func (l *wal) close() error {
	err := l.w.Flush()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// abandon closes the file without flushing what is still in the buffer,
// which is what a crash does.
func (l *wal) abandon() { l.f.Close() }

// replayWAL calls fn for every intact record. A log whose tail was cut in
// the middle of a record (the process died while writing it) is read up to
// the last complete record; `torn` reports that this happened.
func replayWAL(path string, fn func(op byte, key string, value []byte)) (records int, torn bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 256<<10)
	var head [8]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return records, !errors.Is(err, io.EOF), nil
		}
		n := binary.LittleEndian.Uint32(head[4:])
		if n == 0 || n > 64<<20 {
			return records, true, nil
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			return records, true, nil
		}
		if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(head[0:]) {
			return records, true, nil
		}
		klen, used := binary.Uvarint(payload[1:])
		if used <= 0 || 1+used+int(klen) > len(payload) {
			return records, true, nil
		}
		key := string(payload[1+used : 1+used+int(klen)])
		fn(payload[0], key, payload[1+used+int(klen):])
		records++
	}
}
