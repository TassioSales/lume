package lume

import (
	"fmt"
	"os"
	"time"
)

// background flushes frozen memory tables and compacts levels that are over their limit.
func (d *DB) background() {
	defer close(d.done)
	for range d.work {
		if err := d.drain(); err != nil {
			d.mu.Lock()
			d.bgErr = err
			d.flushed.Broadcast()
			d.mu.Unlock()
			return
		}
	}
}

func (d *DB) drain() error {
	for {
		d.mu.RLock()
		imm, closed := d.imm, d.closed
		d.mu.RUnlock()
		if imm != nil {
			if err := d.flushImm(imm); err != nil {
				return err
			}
			continue
		}
		if closed {
			return nil
		}
		did, err := d.compactOnce()
		if err != nil || !did {
			return err
		}
	}
}

// writeTable writes everything an iterator yields into one new table file.
func (d *DB) writeTable(it iterator, level int) (TableMeta, error) {
	d.mu.Lock()
	num := d.next
	d.next++
	d.mu.Unlock()
	w, err := newTableWriter(d.tablePath(num), num, level)
	if err != nil {
		return TableMeta{}, err
	}
	for it.next() {
		if err := w.add(it.key(), it.value(), it.deleted()); err != nil {
			return TableMeta{}, err
		}
	}
	return w.finish()
}

// flushImm turns the frozen memory table into a level 0 file. Only after
// the file is safely on disk and in the manifest is its log deleted.
func (d *DB) flushImm(imm *memtable) error {
	start := time.Now()
	meta, err := d.writeTable(imm.iter(""), 0)
	if err != nil {
		return err
	}
	t, err := openTable(d.tablePath(meta.Num), meta)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.levels[0] = append([]*table{t}, d.levels[0]...)
	if err := d.saveManifest(); err != nil {
		return err
	}
	os.Remove(d.walPath(d.immLog))
	d.imm = nil
	d.stats.Flushes++
	d.stats.TableBytes += meta.Size
	d.event(Event{Kind: "flush", Detail: fmt.Sprintf("memtable gravada como a tabela %d", meta.Num), Outputs: []TableMeta{meta}, Millis: ms(start)})
	d.flushed.Broadcast()
	return nil
}

// limit is how much a level may hold: a number of files for level 0, bytes for the rest.
func (d *DB) limit(level int) int64 {
	if level == 0 {
		return int64(d.opt.L0Trigger)
	}
	size := d.opt.BaseLevelBytes
	for l := 1; l < level; l++ {
		size *= 10
	}
	return size
}

func levelBytes(tables []*table) int64 {
	var n int64
	for _, t := range tables {
		n += t.meta.Size
	}
	return n
}

func overlaps(t *table, min, max string) bool { return t.meta.Max >= min && t.meta.Min <= max }

// compactOnce merges one over-full level into the next and reports
// whether there was anything to do.
//
// Level 0 files overlap each other (each is a snapshot of the memory
// table), so all of them go down together. From level 1 on, files of a
// level don't overlap: one file is picked, together with the files of the
// next level that cover its key range, and they are rewritten as new files
// of the next level.
func (d *DB) compactOnce() (bool, error) {
	d.mu.RLock()
	level := -1
	var upper []*table
	switch {
	case len(d.levels[0]) >= d.opt.L0Trigger:
		level, upper = 0, append([]*table{}, d.levels[0]...)
	default:
		for l := 1; l < numLevels-1; l++ {
			if levelBytes(d.levels[l]) > d.limit(l) {
				// Take turns through the key space, so the whole level gets compacted over time.
				pick := d.levels[l][0]
				for _, t := range d.levels[l] {
					if t.meta.Min > d.cursor[l] {
						pick = t
						break
					}
				}
				level, upper = l, []*table{pick}
				break
			}
		}
	}
	if level < 0 {
		d.mu.RUnlock()
		return false, nil
	}
	min, max := upper[0].meta.Min, upper[0].meta.Max
	for _, t := range upper {
		if t.meta.Min < min {
			min = t.meta.Min
		}
		if t.meta.Max > max {
			max = t.meta.Max
		}
	}
	var lower []*table
	for _, t := range d.levels[level+1] {
		if overlaps(t, min, max) {
			lower = append(lower, t)
		}
	}
	// A tombstone can be dropped only when nothing older can exist below.
	bottom := true
	for l := level + 2; l < numLevels; l++ {
		for _, t := range d.levels[l] {
			if overlaps(t, min, max) {
				bottom = false
			}
		}
	}
	d.mu.RUnlock()

	start := time.Now()
	// Newest first: level 0 files in order, then the level below.
	var its []iterator
	var inputs []TableMeta
	read := 0
	for _, t := range append(append([]*table{}, upper...), lower...) {
		its = append(its, t.iter(""))
		inputs = append(inputs, t.meta)
		read += t.meta.Entries
	}
	var outputs []TableMeta
	var w *tableWriter
	written := 0
	finish := func() error {
		if w == nil {
			return nil
		}
		meta, err := w.finish()
		w = nil
		outputs = append(outputs, meta)
		return err
	}
	for m := newMerge(its); m.next(); {
		if m.deleted() && bottom {
			continue
		}
		if w == nil {
			d.mu.Lock()
			num := d.next
			d.next++
			d.mu.Unlock()
			var err error
			if w, err = newTableWriter(d.tablePath(num), num, level+1); err != nil {
				return false, err
			}
		}
		if err := w.add(m.key(), m.value(), m.deleted()); err != nil {
			return false, err
		}
		written++
		if w.estimated() >= d.opt.TableBytes {
			if err := finish(); err != nil {
				return false, err
			}
		}
	}
	if err := finish(); err != nil {
		return false, err
	}
	for _, it := range its {
		if err := it.(*tableIter).err; err != nil {
			return false, err
		}
	}
	opened := make([]*table, len(outputs))
	for i, meta := range outputs {
		t, err := openTable(d.tablePath(meta.Num), meta)
		if err != nil {
			return false, err
		}
		opened[i] = t
	}

	// Swap the inputs for the outputs in one step, under the lock.
	d.mu.Lock()
	defer d.mu.Unlock()
	gone := map[uint64]bool{}
	for _, t := range inputs {
		gone[t.Num] = true
	}
	for l := level; l <= level+1; l++ {
		kept := d.levels[l][:0:0]
		for _, t := range d.levels[l] {
			if !gone[t.meta.Num] {
				kept = append(kept, t)
			}
		}
		d.levels[l] = kept
	}
	d.levels[level+1] = append(d.levels[level+1], opened...)
	d.sortLevels()
	d.cursor[level] = max
	if err := d.saveManifest(); err != nil {
		return false, err
	}
	// The manifest no longer mentions the inputs: now they can go.
	for _, t := range append(upper, lower...) {
		t.f.Close()
		os.Remove(d.tablePath(t.meta.Num))
	}
	d.stats.Compactions++
	for _, meta := range outputs {
		d.stats.TableBytes += meta.Size
	}
	d.event(Event{
		Kind:    "compaction",
		Detail:  fmt.Sprintf("nível %d → nível %d", level, level+1),
		Inputs:  inputs,
		Outputs: outputs,
		Dropped: read - written,
		Millis:  ms(start),
	})
	return true, nil
}
