// Command lume serves the store with its dashboard, and measures it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"lume"
	"lume/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "bench" {
		if err := bench(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "erro:", err)
			os.Exit(1)
		}
		return
	}
	addr := flag.String("addr", "127.0.0.1:6767", "endereço HTTP")
	dir := flag.String("dir", "data", "pasta dos arquivos do banco")
	sync := flag.Bool("sync", false, "fsync a cada escrita")
	big := flag.Bool("big", false, "tamanhos de produção (4 MB por tabela) em vez dos pequenos da demonstração")
	flag.Parse()

	// Small sizes by default: flushes and compactions happen after a few thousand writes.
	opt := lume.Options{MemtableBytes: 96 << 10, L0Trigger: 4, BaseLevelBytes: 384 << 10, TableBytes: 96 << 10, Sync: *sync}
	if *big {
		opt = lume.Options{Sync: *sync}
	}
	srv, err := server.New(*dir, opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()
	slog.Info("lume no ar", "url", "http://"+*addr, "pasta", *dir)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(1)
	}
	srv.Close()
}

func bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("n", 500_000, "quantas chaves")
	fs.Parse(args)
	value := []byte(strings.Repeat("v", 100))
	key := func(i int) string { return fmt.Sprintf("chave-%09d", i) }
	rate := func(d time.Duration, ops int) string { return fmt.Sprintf("%8.0f ops/s", float64(ops)/d.Seconds()) }

	for _, sync := range []bool{false, true} {
		dir, err := os.MkdirTemp("", "lume-bench")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		db, err := lume.Open(dir, lume.Options{Sync: sync})
		if err != nil {
			return err
		}
		writes := *n
		if sync {
			writes = min(*n, 2000) // each one waits for the disk
		}
		start := time.Now()
		for i := 0; i < writes; i++ {
			if err := db.Put(key(i), value); err != nil {
				return err
			}
		}
		label := "escrita, sem fsync"
		if sync {
			label = "escrita, com fsync"
		}
		fmt.Printf("%-34s %s  (%d chaves de 115 bytes)\n", label, rate(time.Since(start), writes), writes)
		if sync {
			db.Close()
			continue
		}

		ops := make([]lume.Op, 0, 1000)
		start = time.Now()
		for i := 0; i < *n; i++ {
			ops = append(ops, lume.Op{Key: key(*n + i), Value: value})
			if len(ops) == cap(ops) {
				if err := db.Batch(ops); err != nil {
					return err
				}
				ops = ops[:0]
			}
		}
		fmt.Printf("%-34s %s\n", "escrita em lotes de 1.000", rate(time.Since(start), *n))
		if err := db.Flush(); err != nil {
			return err
		}

		const reads = 200_000
		start = time.Now()
		for i := 0; i < reads; i++ {
			if _, err := db.Get(key(rand.IntN(2 * *n))); err != nil {
				return err
			}
		}
		fmt.Printf("%-34s %s\n", "leitura aleatória, chave existe", rate(time.Since(start), reads))
		start = time.Now()
		for i := 0; i < reads; i++ {
			db.Get(key(rand.IntN(2**n)) + "x") // inside the key range: only the filter can rule it out
		}
		fmt.Printf("%-34s %s\n", "leitura de chave que não existe", rate(time.Since(start), reads))
		start = time.Now()
		pairs, _ := db.Scan(key(1000), "", 100_000)
		fmt.Printf("%-34s %s\n", "varredura em ordem", rate(time.Since(start), len(pairs)))

		v := db.View()
		tables, bytes := 0, int64(0)
		for _, level := range v.Levels {
			for _, t := range level {
				tables++
				bytes += t.Size
			}
		}
		fmt.Printf("\n%d tabelas, %.1f MB em disco, %d compactações\n", tables, float64(bytes)/(1<<20), v.Stats.Compactions)
		fmt.Printf("amplificação de escrita: %.1f×  (bytes gravados em tabelas / bytes pedidos)\n", float64(v.Stats.TableBytes)/float64(v.Stats.UserBytes))
		fmt.Printf("leituras de disco evitadas pelo filtro de Bloom: %d; leituras em vão: %d\n\n", v.Stats.BloomSkips, v.Stats.WastedReads)

		db.Close()
		start = time.Now()
		db, err = lume.Open(dir, lume.Options{})
		if err != nil {
			return err
		}
		fmt.Printf("%-34s %v\n\n", "reabrir o banco", time.Since(start).Round(time.Millisecond))
		db.Close()
	}
	return nil
}
