package lume

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"os"
	"sort"
)

// A sorted string table is an immutable file of entries in key order:
//
//	data block | data block | … | index | bloom filter | footer
//
// Entries are grouped in blocks of about 4 KB, each followed by a
// checksum. The index has the last key of every block, so finding a key
// costs one binary search in memory and one block read from disk. The
// bloom filter answers "this key is definitely not here" without reading
// anything.
const (
	blockSize   = 4 << 10
	tableMagic  = 0x6c756d65_73737431 // "lumesst1"
	footerSize  = 40
	bitsPerKey  = 10
	bloomHashes = 7
)

// TableMeta describes a table file.
type TableMeta struct {
	Num        uint64 `json:"num"`
	Level      int    `json:"level"`
	Size       int64  `json:"size"`
	Entries    int    `json:"entries"`
	Tombstones int    `json:"tombstones"`
	Min        string `json:"min"`
	Max        string `json:"max"`
}

type blockRef struct {
	last   string
	offset int64
	length int
}

// bloom is a set that can say "no" for sure and "maybe" otherwise.
type bloom []byte

func bloomHash(key string) (uint32, uint32) {
	h := fnv.New64a()
	h.Write([]byte(key))
	v := h.Sum64()
	return uint32(v), uint32(v>>32) | 1
}

func newBloom(keys int) bloom {
	return make(bloom, max(8, keys*bitsPerKey/8+1))
}

func (b bloom) add(key string) {
	h1, h2 := bloomHash(key)
	bits := uint32(len(b) * 8)
	for i := uint32(0); i < bloomHashes; i++ {
		p := (h1 + i*h2) % bits
		b[p/8] |= 1 << (p % 8)
	}
}

func (b bloom) mayContain(key string) bool {
	h1, h2 := bloomHash(key)
	bits := uint32(len(b) * 8)
	for i := uint32(0); i < bloomHashes; i++ {
		p := (h1 + i*h2) % bits
		if b[p/8]&(1<<(p%8)) == 0 {
			return false
		}
	}
	return true
}

// tableWriter builds a table file from entries given in key order.
type tableWriter struct {
	f      *os.File
	w      *bufio.Writer
	offset int64
	block  []byte
	last   string
	index  []blockRef
	keys   []string
	meta   TableMeta
}

func newTableWriter(path string, num uint64, level int) (*tableWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &tableWriter{f: f, w: bufio.NewWriterSize(f, 256<<10), meta: TableMeta{Num: num, Level: level}}, nil
}

func (t *tableWriter) add(key string, value []byte, del bool) error {
	if t.meta.Entries == 0 {
		t.meta.Min = key
	}
	t.meta.Max = key
	t.meta.Entries++
	t.block = binary.AppendUvarint(t.block, uint64(len(key)))
	if del {
		t.meta.Tombstones++
		t.block = binary.AppendUvarint(t.block, 0)
	} else {
		t.block = binary.AppendUvarint(t.block, uint64(len(value))+1)
	}
	t.block = append(t.block, key...)
	t.block = append(t.block, value...)
	t.last = key
	t.keys = append(t.keys, key)
	if len(t.block) >= blockSize {
		return t.flushBlock()
	}
	return nil
}

func (t *tableWriter) write(b []byte) error {
	n, err := t.w.Write(b)
	t.offset += int64(n)
	return err
}

func (t *tableWriter) flushBlock() error {
	if len(t.block) == 0 {
		return nil
	}
	t.index = append(t.index, blockRef{last: t.last, offset: t.offset, length: len(t.block)})
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], crc32.ChecksumIEEE(t.block))
	if err := t.write(t.block); err != nil {
		return err
	}
	t.block = t.block[:0]
	return t.write(sum[:])
}

// estimated is the size the file would have if finished now.
func (t *tableWriter) estimated() int64 { return t.offset + int64(len(t.block)) }

func (t *tableWriter) finish() (TableMeta, error) {
	if err := t.flushBlock(); err != nil {
		return t.meta, err
	}
	indexOff := t.offset
	var idx []byte
	for _, b := range t.index {
		idx = binary.AppendUvarint(idx, uint64(len(b.last)))
		idx = append(idx, b.last...)
		idx = binary.AppendUvarint(idx, uint64(b.offset))
		idx = binary.AppendUvarint(idx, uint64(b.length))
	}
	if err := t.write(idx); err != nil {
		return t.meta, err
	}
	bloomOff := t.offset
	filter := newBloom(len(t.keys))
	for _, k := range t.keys {
		filter.add(k)
	}
	if err := t.write(filter); err != nil {
		return t.meta, err
	}
	var foot [footerSize]byte
	binary.LittleEndian.PutUint64(foot[0:], uint64(indexOff))
	binary.LittleEndian.PutUint64(foot[8:], uint64(len(idx)))
	binary.LittleEndian.PutUint64(foot[16:], uint64(bloomOff))
	binary.LittleEndian.PutUint64(foot[24:], uint64(len(filter)))
	binary.LittleEndian.PutUint64(foot[32:], tableMagic)
	if err := t.write(foot[:]); err != nil {
		return t.meta, err
	}
	if err := t.w.Flush(); err != nil {
		return t.meta, err
	}
	// The file must be on disk before the manifest points to it.
	if err := t.f.Sync(); err != nil {
		return t.meta, err
	}
	t.meta.Size = t.offset
	return t.meta, t.f.Close()
}

// table is an open table file: index and filter in memory, data on disk.
type table struct {
	meta  TableMeta
	f     *os.File
	index []blockRef
	bloom bloom
}

func openTable(path string, meta TableMeta) (*table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*table, error) {
		f.Close()
		return nil, fmt.Errorf("lume: tabela %d: %w", meta.Num, err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() < footerSize {
		return fail(errors.New("arquivo curto demais"))
	}
	var foot [footerSize]byte
	if _, err := f.ReadAt(foot[:], st.Size()-footerSize); err != nil {
		return fail(err)
	}
	if binary.LittleEndian.Uint64(foot[32:]) != tableMagic {
		return fail(errors.New("não é uma tabela do lume"))
	}
	indexOff, indexLen := int64(binary.LittleEndian.Uint64(foot[0:])), int(binary.LittleEndian.Uint64(foot[8:]))
	bloomOff, bloomLen := int64(binary.LittleEndian.Uint64(foot[16:])), int(binary.LittleEndian.Uint64(foot[24:]))
	if indexOff < 0 || indexOff+int64(indexLen) > st.Size() || bloomOff+int64(bloomLen) > st.Size() {
		return fail(errors.New("rodapé corrompido"))
	}
	idx := make([]byte, indexLen)
	if _, err := f.ReadAt(idx, indexOff); err != nil {
		return fail(err)
	}
	t := &table{meta: meta, f: f, bloom: make(bloom, bloomLen)}
	if _, err := f.ReadAt(t.bloom, bloomOff); err != nil {
		return fail(err)
	}
	for len(idx) > 0 {
		klen, n := binary.Uvarint(idx)
		idx = idx[n:]
		last := string(idx[:klen])
		idx = idx[klen:]
		off, n := binary.Uvarint(idx)
		idx = idx[n:]
		length, n := binary.Uvarint(idx)
		idx = idx[n:]
		t.index = append(t.index, blockRef{last, int64(off), int(length)})
	}
	return t, nil
}

func (t *table) readBlock(i int) ([]byte, error) {
	ref := t.index[i]
	buf := make([]byte, ref.length+4)
	if _, err := t.f.ReadAt(buf, ref.offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	data := buf[:ref.length]
	if crc32.ChecksumIEEE(data) != binary.LittleEndian.Uint32(buf[ref.length:]) {
		return nil, fmt.Errorf("lume: tabela %d: bloco %d corrompido", t.meta.Num, i)
	}
	return data, nil
}

// entry decodes the first entry of a block and returns the rest.
func entry(b []byte) (key string, value []byte, del bool, rest []byte) {
	klen, n := binary.Uvarint(b)
	b = b[n:]
	vlen, n := binary.Uvarint(b)
	b = b[n:]
	key = string(b[:klen])
	b = b[klen:]
	if vlen == 0 {
		return key, nil, true, b
	}
	return key, b[:vlen-1], false, b[vlen-1:]
}

// lookup is the outcome of looking for a key in one table.
type lookup struct {
	value []byte
	del   bool
	found bool
	// What it cost: whether the filter ruled the key out, and whether a block was read.
	filtered  bool
	blockRead bool
}

func (t *table) get(key string) (lookup, error) {
	if key < t.meta.Min || key > t.meta.Max {
		return lookup{filtered: true}, nil
	}
	if !t.bloom.mayContain(key) {
		return lookup{filtered: true}, nil
	}
	i := sort.Search(len(t.index), func(i int) bool { return t.index[i].last >= key })
	if i == len(t.index) {
		return lookup{}, nil
	}
	data, err := t.readBlock(i)
	if err != nil {
		return lookup{}, err
	}
	for len(data) > 0 {
		k, v, del, rest := entry(data)
		if k == key {
			return lookup{value: v, del: del, found: true, blockRead: true}, nil
		}
		if k > key {
			break
		}
		data = rest
	}
	return lookup{blockRead: true}, nil
}

type tableIter struct {
	t     *table
	block int
	data  []byte
	k     string
	v     []byte
	del   bool
	from  string
	err   error
}

// iter walks the table from the first key >= from.
func (t *table) iter(from string) *tableIter {
	it := &tableIter{t: t, from: from}
	if from != "" {
		it.block = sort.Search(len(t.index), func(i int) bool { return t.index[i].last >= from })
	}
	it.block-- // next() loads the block
	return it
}

func (it *tableIter) next() bool {
	for {
		for len(it.data) == 0 {
			it.block++
			if it.block >= len(it.t.index) {
				return false
			}
			if it.data, it.err = it.t.readBlock(it.block); it.err != nil {
				return false
			}
		}
		it.k, it.v, it.del, it.data = entry(it.data)
		if it.k >= it.from {
			return true
		}
	}
}
func (it *tableIter) key() string   { return it.k }
func (it *tableIter) value() []byte { return it.v }
func (it *tableIter) deleted() bool { return it.del }

// merge reads several sorted sources as one. Sources are given newest
// first: when a key is in more than one, the first one wins and the others
// are skipped. This is the heart of both reading ranges and compaction.
type merge struct {
	its   []iterator
	valid []bool
	cur   int
}

func newMerge(its []iterator) *merge {
	m := &merge{its: its, valid: make([]bool, len(its)), cur: -1}
	for i, it := range its {
		m.valid[i] = it.next()
	}
	return m
}

func (m *merge) next() bool {
	if m.cur >= 0 {
		// Step past the key just returned, in every source that has it.
		k := m.its[m.cur].key()
		for i, it := range m.its {
			if m.valid[i] && it.key() == k {
				m.valid[i] = it.next()
			}
		}
	}
	m.cur = -1
	for i, it := range m.its {
		if m.valid[i] && (m.cur < 0 || it.key() < m.its[m.cur].key()) {
			m.cur = i
		}
	}
	return m.cur >= 0
}
func (m *merge) key() string   { return m.its[m.cur].key() }
func (m *merge) value() []byte { return m.its[m.cur].value() }
func (m *merge) deleted() bool { return m.its[m.cur].deleted() }
