package lume

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// small makes flushes and compactions happen with little data.
var small = Options{MemtableBytes: 8 << 10, L0Trigger: 3, BaseLevelBytes: 32 << 10, TableBytes: 8 << 10}

func open(t testing.TB, dir string, opt Options) *DB {
	t.Helper()
	db, err := Open(dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func key(i int) string { return fmt.Sprintf("chave-%06d", i) }

func mustGet(t testing.TB, db *DB, k, want string) {
	t.Helper()
	got, err := db.Get(k)
	if err != nil || string(got) != want {
		t.Fatalf("Get(%q) = %q, %v; esperava %q", k, got, err, want)
	}
}

func mustMiss(t testing.TB, db *DB, k string) {
	t.Helper()
	if v, err := db.Get(k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%q) = %q, %v; esperava não encontrar", k, v, err)
	}
}

func TestSkipListKeepsOrderAndOverwrites(t *testing.T) {
	m := newMemtable()
	perm := rand.Perm(2000)
	for _, i := range perm {
		m.set(key(i), []byte("v"), false)
	}
	m.set(key(7), []byte("novo"), false)
	m.set(key(8), nil, true)
	if m.count != 2000 {
		t.Fatalf("count %d", m.count)
	}
	if v, del, ok := m.get(key(7)); !ok || del || string(v) != "novo" {
		t.Fatal("sobrescrita")
	}
	if _, del, ok := m.get(key(8)); !ok || !del {
		t.Fatal("marca de apagado")
	}
	prev := ""
	n := 0
	for it := m.iter(""); it.next(); n++ {
		if it.key() <= prev {
			t.Fatalf("fora de ordem: %q depois de %q", it.key(), prev)
		}
		prev = it.key()
	}
	if n != 2000 {
		t.Fatalf("iterou %d", n)
	}
	it := m.iter(key(1500))
	if !it.next() || it.key() != key(1500) {
		t.Fatal("seek")
	}
}

func TestTableRoundTripAndBloom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.sst")
	w, err := newTableWriter(path, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	const n = 5000
	for i := 0; i < n; i++ {
		if i%10 == 3 {
			w.add(key(i*2), nil, true)
		} else {
			w.add(key(i*2), []byte(strings.Repeat("x", i%50)), false)
		}
	}
	meta, err := w.finish()
	if err != nil {
		t.Fatal(err)
	}
	if meta.Entries != n || meta.Tombstones != n/10 || meta.Min != key(0) || meta.Max != key((n-1)*2) {
		t.Fatalf("%+v", meta)
	}
	tb, err := openTable(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.f.Close()
	if len(tb.index) < 10 {
		t.Fatalf("só %d blocos", len(tb.index))
	}
	for _, i := range []int{0, 1, 3, 2500, n - 1} {
		r, err := tb.get(key(i * 2))
		if err != nil || !r.found || r.del != (i%10 == 3) || len(r.value) != map[bool]int{true: 0, false: i % 50}[i%10 == 3] {
			t.Fatalf("chave %d: %+v %v", i, r, err)
		}
	}
	// Odd keys were never written. The filter must rule out almost all of them.
	falsePositives := 0
	for i := 0; i < n; i++ {
		r, _ := tb.get(key(i*2 + 1))
		if r.found {
			t.Fatal("achou uma chave que não existe")
		}
		if !r.filtered {
			falsePositives++
		}
	}
	if rate := float64(falsePositives) / n; rate > 0.02 {
		t.Fatalf("falsos positivos do filtro de Bloom: %.2f%%", rate*100)
	}
	// Iteration from the middle.
	it := tb.iter(key(5001))
	if !it.next() || it.key() != key(5002) {
		t.Fatalf("seek: %q", it.key())
	}
	// A flipped bit in a block is detected.
	raw, _ := os.ReadFile(path)
	raw[100] ^= 1
	os.WriteFile(path, raw, 0o644)
	bad, err := openTable(path, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.f.Close()
	if _, err := bad.get(key(0)); err == nil || !strings.Contains(err.Error(), "corrompido") {
		t.Fatalf("bloco corrompido não detectado: %v", err)
	}
}

func TestMergeNewestWins(t *testing.T) {
	newer, older := newMemtable(), newMemtable()
	older.set("a", []byte("1"), false)
	older.set("b", []byte("1"), false)
	older.set("c", []byte("1"), false)
	newer.set("b", []byte("2"), false)
	newer.set("c", nil, true)
	newer.set("d", []byte("2"), false)
	var got []string
	for m := newMerge([]iterator{newer.iter(""), older.iter("")}); m.next(); {
		got = append(got, fmt.Sprintf("%s=%s%v", m.key(), m.value(), m.deleted()))
	}
	if strings.Join(got, " ") != "a=1false b=2false c=true d=2false" {
		t.Fatalf("%v", got)
	}
}

func TestPutGetDeleteAcrossFlushesAndCompactions(t *testing.T) {
	db := open(t, t.TempDir(), small)
	defer db.Close()
	const n = 4000
	for i := 0; i < n; i++ {
		if err := db.Put(key(i), []byte(fmt.Sprintf("valor-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i += 3 {
		db.Delete(key(i))
	}
	for i := 1; i < n; i += 3 {
		db.Put(key(i), []byte("novo"))
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		switch i % 3 {
		case 0:
			mustMiss(t, db, key(i))
		case 1:
			mustGet(t, db, key(i), "novo")
		default:
			mustGet(t, db, key(i), fmt.Sprintf("valor-%d", i))
		}
	}
	v := db.View()
	if v.Stats.Flushes == 0 || v.Stats.Compactions == 0 {
		t.Fatalf("esperava flushes e compactações: %+v", v.Stats)
	}
	// Levels below 0 never have overlapping files.
	for l := 1; l < len(v.Levels); l++ {
		for i := 1; i < len(v.Levels[l]); i++ {
			if v.Levels[l][i].Min <= v.Levels[l][i-1].Max {
				t.Fatalf("nível %d com arquivos sobrepostos: %+v", l, v.Levels[l])
			}
		}
	}
	// Scan sees one live value per key, in order.
	pairs, err := db.Scan(key(10), key(20), 100)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, p := range pairs {
		keys = append(keys, p.Key)
	}
	want := []string{key(10), key(11), key(13), key(14), key(16), key(17), key(19)}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("scan: %v", keys)
	}
	if pairs[0].Value == nil || string(pairs[0].Value) != "novo" {
		t.Fatalf("scan devolveu valor antigo: %q", pairs[0].Value)
	}
	if limited, _ := db.Scan("", "", 5); len(limited) != 5 {
		t.Fatalf("limite: %d", len(limited))
	}
}

// The store against a plain map, through random writes, deletes, reopens and crashes.
func TestMatchesAModelThroughRestartsAndCrashes(t *testing.T) {
	dir := t.TempDir()
	rnd := rand.New(rand.NewPCG(1, 2))
	model := map[string]string{}
	db := open(t, dir, small)
	for round := 0; round < 12; round++ {
		for i := 0; i < 1500; i++ {
			k := key(rnd.IntN(900))
			if rnd.IntN(4) == 0 {
				db.Delete(k)
				delete(model, k)
			} else {
				v := fmt.Sprintf("r%d-%d-%s", round, i, strings.Repeat("z", rnd.IntN(40)))
				db.Put(k, []byte(v))
				model[k] = v
			}
		}
		switch round % 3 {
		case 0:
			db.Close()
		case 1:
			db.Crash() // nothing flushed: everything must come back from the logs
		default:
			db.Flush()
			db.Close()
		}
		db = open(t, dir, small)
		for i := 0; i < 900; i++ {
			k := key(i)
			if want, ok := model[k]; ok {
				mustGet(t, db, k, want)
			} else {
				mustMiss(t, db, k)
			}
		}
		pairs, _ := db.Scan("", "", 10000)
		if len(pairs) != len(model) {
			t.Fatalf("rodada %d: scan devolveu %d pares, o modelo tem %d", round, len(pairs), len(model))
		}
		if !sort.SliceIsSorted(pairs, func(i, j int) bool { return pairs[i].Key < pairs[j].Key }) {
			t.Fatal("scan fora de ordem")
		}
	}
	db.Close()
}

func TestTornLogTailIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, Options{})
	for i := 0; i < 100; i++ {
		db.Put(key(i), []byte("valor"))
	}
	db.Crash()
	logs, _ := filepath.Glob(filepath.Join(dir, "*.wal"))
	if len(logs) != 1 {
		t.Fatalf("logs: %v", logs)
	}
	// The process died in the middle of writing the last record.
	st, _ := os.Stat(logs[0])
	if err := os.Truncate(logs[0], st.Size()-7); err != nil {
		t.Fatal(err)
	}
	db = open(t, dir, Options{})
	defer db.Close()
	for i := 0; i < 99; i++ {
		mustGet(t, db, key(i), "valor")
	}
	mustMiss(t, db, key(99))
	ev := db.View().Events
	if len(ev) == 0 || ev[0].Kind != "recovery" || !strings.Contains(ev[0].Detail, "99 operações") || !strings.Contains(ev[0].Detail, "cortado") {
		t.Fatalf("evento de recuperação: %+v", ev)
	}
}

func TestCompactionDropsOverwrittenAndDeleted(t *testing.T) {
	dir := t.TempDir()
	db := open(t, dir, small)
	value := []byte(strings.Repeat("v", 100))
	for round := 0; round < 20; round++ {
		for i := 0; i < 300; i++ {
			db.Put(key(i), value)
		}
	}
	for i := 0; i < 300; i++ {
		db.Delete(key(i))
	}
	db.Flush()
	// Push everything down: one more round of writes on other keys triggers the merges.
	for i := 1000; i < 1600; i++ {
		db.Put(key(i), value)
	}
	db.Flush()
	v := db.View()
	live := 0
	for _, level := range v.Levels {
		for _, t := range level {
			live += t.Entries
		}
	}
	dropped := 0
	for _, e := range v.Events {
		dropped += e.Dropped
	}
	if dropped < 5000 {
		t.Fatalf("a compactação descartou só %d entradas de 6.300 obsoletas", dropped)
	}
	if amp := float64(v.Stats.TableBytes) / float64(v.Stats.UserBytes); amp > 12 {
		t.Fatalf("amplificação de escrita %.1f", amp)
	}
	for i := 0; i < 300; i++ {
		mustMiss(t, db, key(i))
	}
	db.Close()
	// No file is left behind that the manifest doesn't know.
	files, _ := filepath.Glob(filepath.Join(dir, "*.sst"))
	known := 0
	for _, level := range v.Levels {
		known += len(level)
	}
	if len(files) != known {
		t.Fatalf("%d arquivos no disco, %d no manifesto (%d entradas vivas)", len(files), known, live)
	}
}

func TestTraceShowsThePathOfARead(t *testing.T) {
	db := open(t, t.TempDir(), small)
	defer db.Close()
	for i := 0; i < 2000; i++ {
		db.Put(key(i), []byte("x"))
	}
	db.Flush()
	db.Put("zz-na-memoria", []byte("y"))
	_, steps, err := db.Trace("zz-na-memoria")
	if err != nil || len(steps) != 1 || steps[0].Where != "memtable" {
		t.Fatalf("%+v %v", steps, err)
	}
	_, steps, err = db.Trace(key(1234))
	if err != nil || steps[0].Result != "não está" || !strings.Contains(steps[len(steps)-1].Result, "achou") {
		t.Fatalf("%+v %v", steps, err)
	}
	_, steps, err = db.Trace("chave-que-nao-existe")
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	for _, s := range steps[1:] {
		if s.Where == "tabela" && !strings.Contains(s.Result, "Bloom") && !strings.Contains(s.Result, "falso positivo") {
			t.Fatalf("passo inesperado: %+v", s)
		}
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	db := open(t, t.TempDir(), small)
	defer db.Close()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1500; i++ {
				if err := db.Put(fmt.Sprintf("w%d-%05d", w, i), []byte("v")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1500; i++ {
				// A key written earlier by writer r must be readable now, whatever is being flushed or compacted.
				k := fmt.Sprintf("w%d-%05d", r, i/2)
				if _, err := db.Get(k); err != nil && !errors.Is(err, ErrNotFound) {
					t.Error(err)
					return
				}
				db.Scan(k, "", 5)
			}
		}()
	}
	wg.Wait()
	for w := 0; w < 4; w++ {
		for i := 0; i < 1500; i += 97 {
			mustGet(t, db, fmt.Sprintf("w%d-%05d", w, i), "v")
		}
	}
}

func TestBatchAndValidation(t *testing.T) {
	db := open(t, t.TempDir(), Options{})
	if err := db.Put("", []byte("x")); err == nil {
		t.Fatal("chave vazia aceita")
	}
	if err := db.Batch([]Op{{Key: "a", Value: []byte("1")}, {Key: "b", Value: []byte("2")}, {Key: "a", Delete: true}}); err != nil {
		t.Fatal(err)
	}
	mustMiss(t, db, "a")
	mustGet(t, db, "b", "2")
	db.Close()
	if err := db.Put("c", nil); err == nil {
		t.Fatal("escrita depois de fechar")
	}
}

func BenchmarkPut(b *testing.B) {
	db := open(b, b.TempDir(), Options{})
	defer db.Close()
	value := []byte(strings.Repeat("v", 100))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Put(key(i), value)
	}
}

func BenchmarkGet(b *testing.B) {
	db := open(b, b.TempDir(), Options{})
	defer db.Close()
	value := []byte(strings.Repeat("v", 100))
	const n = 200_000
	for i := 0; i < n; i++ {
		db.Put(key(i), value)
	}
	db.Flush()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.Get(key(i % n))
	}
}
