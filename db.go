package lume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options tune the store. The zero value works; the demo uses small sizes
// so that flushes and compactions happen while you watch.
type Options struct {
	// MemtableBytes is how much the memory table holds before it is flushed. Default 4 MB.
	MemtableBytes int
	// L0Trigger is how many files level 0 may have before they are merged down. Default 4.
	L0Trigger int
	// BaseLevelBytes is the size limit of level 1; each level below holds 10 times more. Default 32 MB.
	BaseLevelBytes int64
	// TableBytes is the target size of a file produced by compaction. Default 4 MB.
	TableBytes int64
	// Sync makes every write wait for the disk (fsync). Without it, a write
	// survives the process dying, but not the machine losing power.
	Sync bool
}

func (o *Options) defaults() {
	if o.MemtableBytes <= 0 {
		o.MemtableBytes = 4 << 20
	}
	if o.L0Trigger <= 0 {
		o.L0Trigger = 4
	}
	if o.BaseLevelBytes <= 0 {
		o.BaseLevelBytes = 32 << 20
	}
	if o.TableBytes <= 0 {
		o.TableBytes = 4 << 20
	}
}

const numLevels = 6

// ErrNotFound is returned by Get when the key has no value.
var ErrNotFound = errors.New("lume: key not found")

// Event is something the store did by itself: a flush, a compaction, a recovery.
type Event struct {
	At      time.Time   `json:"at"`
	Kind    string      `json:"kind"`
	Detail  string      `json:"detail"`
	Inputs  []TableMeta `json:"inputs,omitempty"`
	Outputs []TableMeta `json:"outputs,omitempty"`
	// Entries that were dropped: overwritten values and deletions that reached the bottom.
	Dropped int     `json:"dropped"`
	Millis  float64 `json:"millis"`
}

// Stats counts what happened since the store was opened.
type Stats struct {
	Puts        int64 `json:"puts"`
	Deletes     int64 `json:"deletes"`
	Gets        int64 `json:"gets"`
	Hits        int64 `json:"hits"`
	BloomSkips  int64 `json:"bloomSkips"`
	BlockReads  int64 `json:"blockReads"`
	WastedReads int64 `json:"wastedReads"`
	Flushes     int64 `json:"flushes"`
	Compactions int64 `json:"compactions"`
	// Bytes the application asked to write, and bytes actually written to
	// table files. Their ratio is the write amplification.
	UserBytes  int64 `json:"userBytes"`
	TableBytes int64 `json:"tableBytes"`
	WALBytes   int64 `json:"walBytes"`
}

type DB struct {
	dir string
	opt Options

	// writeMu serializes writers; mu guards the structure below.
	writeMu sync.Mutex
	mu      sync.RWMutex
	flushed *sync.Cond
	mem     *memtable
	imm     *memtable // being written to disk
	log     *wal
	logNum  uint64
	immLog  uint64
	levels  [numLevels][]*table // level 0 newest first; the others sorted by key, not overlapping
	next    uint64
	cursor  [numLevels]string // where the last compaction of each level stopped
	stats   Stats
	// Readers update counters while holding only the read lock.
	statsMu sync.Mutex
	events  []Event
	closed  bool
	bgErr   error

	work chan struct{}
	done chan struct{}
}

type manifest struct {
	Next   uint64      `json:"next"`
	Tables []TableMeta `json:"tables"`
}

func (d *DB) tablePath(num uint64) string { return filepath.Join(d.dir, fmt.Sprintf("%06d.sst", num)) }
func (d *DB) walPath(num uint64) string   { return filepath.Join(d.dir, fmt.Sprintf("%06d.wal", num)) }

// Open opens the store in dir, creating it if needed, and recovers
// whatever a previous run left in its logs.
func Open(dir string, opt Options) (*DB, error) {
	opt.defaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	d := &DB{dir: dir, opt: opt, mem: newMemtable(), next: 1, work: make(chan struct{}, 1), done: make(chan struct{})}
	d.flushed = sync.NewCond(&d.mu)

	var m manifest
	if raw, err := os.ReadFile(filepath.Join(dir, "MANIFEST")); err == nil {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("lume: manifesto ilegível: %w", err)
		}
		d.next = m.Next
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	live := map[string]bool{}
	for _, meta := range m.Tables {
		t, err := openTable(d.tablePath(meta.Num), meta)
		if err != nil {
			d.closeTables()
			return nil, err
		}
		d.levels[meta.Level] = append(d.levels[meta.Level], t)
		live[filepath.Base(d.tablePath(meta.Num))] = true
	}
	d.sortLevels()

	// Files the manifest doesn't know are leftovers of a flush or compaction
	// that didn't finish: the manifest is the truth.
	names, _ := os.ReadDir(dir)
	var logs []string
	for _, e := range names {
		switch {
		case strings.HasSuffix(e.Name(), ".sst") && !live[e.Name()], strings.HasSuffix(e.Name(), ".tmp"):
			os.Remove(filepath.Join(dir, e.Name()))
		case strings.HasSuffix(e.Name(), ".wal"):
			logs = append(logs, e.Name())
		}
	}
	sort.Strings(logs)
	start := time.Now()
	records, torn := 0, false
	for _, name := range logs {
		n, t, err := replayWAL(filepath.Join(dir, name), func(op byte, key string, value []byte) {
			d.mem.set(key, append([]byte(nil), value...), op == opDel)
		})
		if err != nil {
			d.closeTables()
			return nil, err
		}
		records += n
		torn = torn || t
	}
	if records > 0 {
		// What was only in the logs goes to a table right away, so the logs can go.
		meta, err := d.writeTable(d.mem.iter(""), 0)
		if err != nil {
			d.closeTables()
			return nil, err
		}
		t, err := openTable(d.tablePath(meta.Num), meta)
		if err != nil {
			return nil, err
		}
		d.levels[0] = append([]*table{t}, d.levels[0]...)
		d.mem = newMemtable()
		detail := fmt.Sprintf("%d operações recuperadas do log", records)
		if torn {
			detail += "; o fim do log estava cortado no meio de um registro e foi descartado"
		}
		d.event(Event{Kind: "recovery", Detail: detail, Outputs: []TableMeta{meta}, Millis: ms(start)})
	}
	if err := d.saveManifest(); err != nil {
		d.closeTables()
		return nil, err
	}
	for _, name := range logs {
		os.Remove(filepath.Join(dir, name))
	}
	if err := d.newLog(); err != nil {
		d.closeTables()
		return nil, err
	}
	go d.background()
	d.kick()
	return d, nil
}

func ms(since time.Time) float64 { return float64(time.Since(since).Microseconds()) / 1000 }

func (d *DB) newLog() error {
	d.logNum = d.next
	d.next++
	l, err := createWAL(d.walPath(d.logNum), d.opt.Sync)
	d.log = l
	return err
}

func (d *DB) sortLevels() {
	sort.Slice(d.levels[0], func(i, j int) bool { return d.levels[0][i].meta.Num > d.levels[0][j].meta.Num })
	for l := 1; l < numLevels; l++ {
		sort.Slice(d.levels[l], func(i, j int) bool { return d.levels[l][i].meta.Min < d.levels[l][j].meta.Min })
	}
}

// saveManifest records which tables make up the store. It is replaced
// atomically (write a new file, rename over the old one), so a crash
// leaves either the old set of tables or the new one, never a mix.
func (d *DB) saveManifest() error {
	m := manifest{Next: d.next, Tables: []TableMeta{}}
	for _, level := range d.levels {
		for _, t := range level {
			m.Tables = append(m.Tables, t.meta)
		}
	}
	raw, _ := json.MarshalIndent(m, "", " ")
	tmp := filepath.Join(d.dir, "MANIFEST.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d.dir, "MANIFEST"))
}

func (d *DB) event(e Event) {
	e.At = time.Now()
	d.events = append(d.events, e)
	if len(d.events) > 200 {
		d.events = d.events[len(d.events)-200:]
	}
}

func (d *DB) kick() {
	select {
	case d.work <- struct{}{}:
	default:
	}
}

// ---- writing ----------------------------------------------------------------

// Put stores a value.
func (d *DB) Put(key string, value []byte) error { return d.write(key, value, false) }

// Delete removes a key. It writes a tombstone, which hides older values
// until compaction throws both away.
func (d *DB) Delete(key string) error { return d.write(key, nil, true) }

// Op is one change of a batch.
type Op struct {
	Key    string
	Value  []byte
	Delete bool
}

// Batch applies several changes with a single trip to the log.
func (d *DB) Batch(ops []Op) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.apply(ops)
}

func (d *DB) write(key string, value []byte, del bool) error {
	if key == "" {
		return errors.New("lume: empty key")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.apply([]Op{{key, value, del}})
}

func (d *DB) apply(ops []Op) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errors.New("lume: store is closed")
	}
	if d.bgErr != nil {
		d.mu.Unlock()
		return d.bgErr
	}
	// The memory table is full and the previous one is still going to disk: wait for it.
	for d.mem.size >= d.opt.MemtableBytes && d.imm != nil && !d.closed {
		d.flushed.Wait()
	}
	log := d.log
	d.mu.Unlock()

	// Log first: if the process dies after this, the change is recovered.
	for _, op := range ops {
		kind := opPut
		if op.Delete {
			kind = opDel
		}
		log.append(kind, op.Key, op.Value)
	}
	if err := log.commit(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, op := range ops {
		d.mem.set(op.Key, op.Value, op.Delete)
		if op.Delete {
			d.stats.Deletes++
		} else {
			d.stats.Puts++
		}
		d.stats.UserBytes += int64(len(op.Key) + len(op.Value))
	}
	d.stats.WALBytes = log.size
	if d.mem.size >= d.opt.MemtableBytes && d.imm == nil {
		return d.rotate()
	}
	return nil
}

// rotate freezes the memory table and starts a new one with a new log.
// The frozen table is written to disk in the background. Called with mu held.
func (d *DB) rotate() error {
	old := d.log
	d.imm, d.immLog = d.mem, d.logNum
	d.mem = newMemtable()
	if err := d.newLog(); err != nil {
		return err
	}
	d.kick()
	return old.close()
}

// Flush writes the memory table to disk now and waits for it.
func (d *DB) Flush() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.imm != nil {
		d.flushed.Wait()
	}
	if d.mem.count == 0 {
		return nil
	}
	if err := d.rotate(); err != nil {
		return err
	}
	for d.imm != nil && d.bgErr == nil {
		d.flushed.Wait()
	}
	return d.bgErr
}

// ---- reading ----------------------------------------------------------------

// Step is one place Get looked, for showing the path of a read.
type Step struct {
	Where  string `json:"where"`
	Level  int    `json:"level"`
	Table  uint64 `json:"table,omitempty"`
	Result string `json:"result"`
}

// Get returns the value of a key.
func (d *DB) Get(key string) ([]byte, error) {
	v, _, err := d.get(key, false)
	return v, err
}

// Trace is Get, plus every place it looked on the way.
func (d *DB) Trace(key string) ([]byte, []Step, error) { return d.get(key, true) }

func (d *DB) get(key string, trace bool) ([]byte, []Step, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var steps []Step
	var gets, hits, skips, reads, wasted int64 = 1, 0, 0, 0, 0
	defer func() {
		// Counters are updated under the read lock by many readers: use a tiny critical section.
		d.statsMu.Lock()
		d.stats.Gets += gets
		d.stats.Hits += hits
		d.stats.BloomSkips += skips
		d.stats.BlockReads += reads
		d.stats.WastedReads += wasted
		d.statsMu.Unlock()
	}()
	note := func(where string, level int, num uint64, result string) {
		if trace {
			steps = append(steps, Step{where, level, num, result})
		}
	}
	answer := func(v []byte, del bool) ([]byte, []Step, error) {
		if del {
			return nil, steps, ErrNotFound
		}
		hits = 1
		return append([]byte(nil), v...), steps, nil
	}

	// Newest data first: the first place that knows the key has the answer.
	for i, m := range []*memtable{d.mem, d.imm} {
		if m == nil {
			continue
		}
		where := []string{"memtable", "memtable em gravação"}[i]
		if v, del, ok := m.get(key); ok {
			note(where, -1, 0, found(del))
			return answer(v, del)
		}
		note(where, -1, 0, "não está")
	}
	for level, tables := range d.levels {
		candidates := tables
		if level > 0 {
			// Files of a level don't overlap: at most one can have the key.
			i := sort.Search(len(tables), func(i int) bool { return tables[i].meta.Max >= key })
			if i == len(tables) || tables[i].meta.Min > key {
				continue
			}
			candidates = tables[i : i+1]
		}
		for _, t := range candidates {
			r, err := t.get(key)
			if err != nil {
				return nil, steps, err
			}
			switch {
			case r.filtered:
				skips++
				note("tabela", level, t.meta.Num, "o filtro de Bloom garante que não está: nenhuma leitura de disco")
			case r.found:
				reads++
				note("tabela", level, t.meta.Num, "leu um bloco do disco: "+found(r.del))
				return answer(r.value, r.del)
			default:
				reads++
				wasted++
				note("tabela", level, t.meta.Num, "o filtro disse talvez, leu um bloco e não estava (falso positivo)")
			}
		}
	}
	return nil, steps, ErrNotFound
}

func found(del bool) string {
	if del {
		return "achou uma marca de apagado"
	}
	return "achou"
}

// Pair is a key with its value.
type Pair struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

// Scan returns up to limit pairs with from <= key < to, in key order.
// An empty `to` means no upper bound.
func (d *DB) Scan(from, to string, limit int) ([]Pair, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	its := []iterator{d.mem.iter(from)}
	if d.imm != nil {
		its = append(its, d.imm.iter(from))
	}
	for _, level := range d.levels {
		for _, t := range level {
			if (to == "" || t.meta.Min < to) && t.meta.Max >= from {
				its = append(its, t.iter(from))
			}
		}
	}
	out := []Pair{}
	for m := newMerge(its); m.next() && len(out) < limit; {
		if to != "" && m.key() >= to {
			break
		}
		if !m.deleted() {
			out = append(out, Pair{m.key(), append([]byte(nil), m.value()...)})
		}
	}
	return out, nil
}

// ---- inspection -------------------------------------------------------------

// View is a picture of the store for the dashboard.
type View struct {
	Memtable struct {
		Entries int `json:"entries"`
		Bytes   int `json:"bytes"`
		Limit   int `json:"limit"`
	} `json:"memtable"`
	Flushing bool          `json:"flushing"`
	Levels   [][]TableMeta `json:"levels"`
	// Limit of each level: files for level 0, bytes for the others.
	Limits []int64 `json:"limits"`
	Stats  Stats   `json:"stats"`
	Events []Event `json:"events"`
}

func (d *DB) View() View {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var v View
	v.Memtable.Entries, v.Memtable.Bytes, v.Memtable.Limit = d.mem.count, d.mem.size, d.opt.MemtableBytes
	v.Flushing = d.imm != nil
	for l, level := range d.levels {
		metas := make([]TableMeta, len(level))
		for i, t := range level {
			metas[i] = t.meta
		}
		v.Levels = append(v.Levels, metas)
		v.Limits = append(v.Limits, d.limit(l))
	}
	d.statsMu.Lock()
	v.Stats = d.stats
	d.statsMu.Unlock()
	v.Events = append([]Event{}, d.events...)
	return v
}

// ---- closing ----------------------------------------------------------------

// Close flushes the log and stops the background work. What is in the
// memory table stays in the log and is recovered at the next Open.
func (d *DB) Close() error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.flushed.Broadcast()
	d.mu.Unlock()
	close(d.work)
	<-d.done
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.log.close()
	d.closeTables()
	return err
}

// Crash stops the store the way a killed process would: nothing is
// flushed, nothing is closed properly. For demonstrations and tests.
func (d *DB) Crash() {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.flushed.Broadcast()
	d.mu.Unlock()
	close(d.work)
	<-d.done
	d.mu.Lock()
	defer d.mu.Unlock()
	d.log.abandon()
	d.closeTables()
}

func (d *DB) closeTables() {
	for _, level := range d.levels {
		for _, t := range level {
			t.f.Close()
		}
	}
}
