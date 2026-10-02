package lume

import "math/rand/v2"

const maxHeight = 12

type node struct {
	key   string
	value []byte
	del   bool
	next  []*node
}

// memtable holds the most recent writes, sorted, in a skip list: a linked
// list with express lanes. Each node is in lane 0, and in each lane above
// with probability 1/4, so a search skips most of the list.
type memtable struct {
	head   *node
	height int
	count  int
	// Approximate bytes held, to decide when to flush.
	size int
}

func newMemtable() *memtable {
	return &memtable{head: &node{next: make([]*node, maxHeight)}, height: 1}
}

// set stores a value, or a tombstone when del is true. A tombstone has to
// be kept: it is what hides older values of the key in the files on disk.
func (m *memtable) set(key string, value []byte, del bool) {
	var prev [maxHeight]*node
	x := m.head
	for lane := m.height - 1; lane >= 0; lane-- {
		for x.next[lane] != nil && x.next[lane].key < key {
			x = x.next[lane]
		}
		prev[lane] = x
	}
	if n := x.next[0]; n != nil && n.key == key {
		m.size += len(value) - len(n.value)
		n.value, n.del = value, del
		return
	}
	h := 1
	for h < maxHeight && rand.IntN(4) == 0 {
		h++
	}
	for lane := m.height; lane < h; lane++ {
		prev[lane] = m.head
	}
	m.height = max(m.height, h)
	n := &node{key: key, value: value, del: del, next: make([]*node, h)}
	for lane := 0; lane < h; lane++ {
		n.next[lane] = prev[lane].next[lane]
		prev[lane].next[lane] = n
	}
	m.count++
	m.size += len(key) + len(value) + 24
}

func (m *memtable) seek(key string) *node {
	x := m.head
	for lane := m.height - 1; lane >= 0; lane-- {
		for x.next[lane] != nil && x.next[lane].key < key {
			x = x.next[lane]
		}
	}
	return x.next[0]
}

func (m *memtable) get(key string) (value []byte, del, found bool) {
	if n := m.seek(key); n != nil && n.key == key {
		return n.value, n.del, true
	}
	return nil, false, false
}

// iterator is how every sorted source (memory table, file, merge) is read.
type iterator interface {
	// next advances and reports whether there is an entry.
	next() bool
	key() string
	value() []byte
	deleted() bool
}

type memIter struct {
	n     *node
	start *node
}

// iter walks the table from the first key >= from.
func (m *memtable) iter(from string) iterator {
	if from == "" {
		return &memIter{start: m.head.next[0]}
	}
	return &memIter{start: m.seek(from)}
}

func (it *memIter) next() bool {
	if it.start != nil {
		it.n, it.start = it.start, nil
		return true
	}
	if it.n == nil {
		return false
	}
	it.n = it.n.next[0]
	return it.n != nil
}
func (it *memIter) key() string   { return it.n.key }
func (it *memIter) value() []byte { return it.n.value }
func (it *memIter) deleted() bool { return it.n.del }
