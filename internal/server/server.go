// Package server lets a browser drive the store and watch what it does.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"lume"
	"lume/ui"
)

type Server struct {
	dir string
	opt lume.Options

	// mu lets a crash or a reset swap the store while requests wait.
	mu sync.RWMutex
	db *lume.DB
	// Highest id handed out by the load generator.
	next int
}

const counterKey = "~proximo"

func New(dir string, opt lume.Options) (*Server, error) {
	s := &Server{dir: dir, opt: opt}
	return s, s.open()
}

func (s *Server) open() error {
	db, err := lume.Open(s.dir, s.opt)
	if err != nil {
		return err
	}
	s.db = db
	s.next = 0
	if v, err := db.Get(counterKey); err == nil {
		s.next, _ = strconv.Atoi(string(v))
	}
	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/view", s.view)
	mux.HandleFunc("GET /api/get", s.get)
	mux.HandleFunc("GET /api/scan", s.scan)
	mux.HandleFunc("POST /api/put", s.put)
	mux.HandleFunc("POST /api/delete", s.del)
	mux.HandleFunc("POST /api/load", s.load)
	mux.HandleFunc("POST /api/flush", s.flush)
	mux.HandleFunc("POST /api/crash", s.crash)
	mux.HandleFunc("POST /api/reset", s.reset)
	mux.Handle("/", spa())
	return secure(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "JSON inválido.")
		return false
	}
	return true
}

func (s *Server) view(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"view": s.db.View(), "sync": s.opt.Sync, "nextId": s.next})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" || len(key) > 512 {
		fail(w, http.StatusBadRequest, "Informe uma chave.")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	start := time.Now()
	value, steps, err := s.db.Trace(key)
	took := time.Since(start)
	if err != nil && !errors.Is(err, lume.ErrNotFound) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": err == nil, "value": string(value), "steps": steps, "micros": took.Microseconds()})
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if len(prefix) > 512 {
		fail(w, http.StatusBadRequest, "Prefixo longo demais.")
		return
	}
	// Every key that starts with the prefix sorts before prefix + the highest character.
	to := ""
	if prefix != "" {
		to = prefix + "\U0010ffff"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pairs, err := s.db.Scan(prefix, to, 60)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]string, 0, len(pairs))
	for _, p := range pairs {
		if p.Key != counterKey {
			out = append(out, map[string]string{"key": p.Key, "value": string(p.Value)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pairs": out})
}

func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Key == "" || len(in.Key) > 512 || len(in.Value) > 64<<10 {
		fail(w, http.StatusBadRequest, "Chave vazia, ou chave e valor grandes demais.")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.db.Put(in.Key, []byte(in.Value)); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Key == "" || len(in.Key) > 512 {
		fail(w, http.StatusBadRequest, "Informe uma chave.")
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.db.Delete(in.Key); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var names = []string{"Ana", "Bruno", "Carla", "Diego", "Elisa", "Fábio", "Gabi", "Heitor", "Íris", "João", "Karen", "Luan", "Marta", "Nuno", "Olga", "Paulo"}

// UserKey is the key the load generator uses for id n.
func UserKey(n int) string { return fmt.Sprintf("usuario:%07d", n) }

func userValue() []byte {
	v, _ := json.Marshal(map[string]any{"nome": names[rand.IntN(len(names))], "saldo": rand.IntN(100000), "cidade": []string{"Recife", "Belém", "Curitiba", "Manaus", "Goiânia", "Natal"}[rand.IntN(6)]})
	return v
}

// load writes a burst of changes: new keys, overwrites of existing ones, or deletions.
func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind string `json:"kind"`
		N    int    `json:"n"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.N < 1 || in.N > 50000 {
		fail(w, http.StatusBadRequest, "Entre 1 e 50.000 operações.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.Kind != "new" && s.next == 0 {
		fail(w, http.StatusConflict, "Ainda não há chaves para sobrescrever ou apagar.")
		return
	}
	start := time.Now()
	ops := make([]lume.Op, 0, 500)
	flush := func() error {
		if len(ops) == 0 {
			return nil
		}
		err := s.db.Batch(ops)
		ops = ops[:0]
		return err
	}
	for i := 0; i < in.N; i++ {
		switch in.Kind {
		case "new":
			s.next++
			ops = append(ops, lume.Op{Key: UserKey(s.next), Value: userValue()})
		case "overwrite":
			ops = append(ops, lume.Op{Key: UserKey(1 + rand.IntN(s.next)), Value: userValue()})
		case "delete":
			ops = append(ops, lume.Op{Key: UserKey(1 + rand.IntN(s.next)), Delete: true})
		default:
			fail(w, http.StatusBadRequest, "Tipo de carga desconhecido.")
			return
		}
		if len(ops) == cap(ops) {
			if err := flush(); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	ops = append(ops, lume.Op{Key: counterKey, Value: []byte(strconv.Itoa(s.next))})
	if err := flush(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"millis": float64(time.Since(start).Microseconds()) / 1000})
}

func (s *Server) flush(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.db.Flush(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// crash kills the store without warning and opens it again, the way a
// supervisor would restart a dead process. What was only in memory has to
// come back from the log.
func (s *Server) crash(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lost := s.db.View().Memtable.Entries
	s.db.Crash()
	start := time.Now()
	if err := s.open(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"inMemory": lost, "millis": float64(time.Since(start).Microseconds()) / 1000})
}

func (s *Server) reset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Close()
	if err := os.RemoveAll(s.dir); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.open(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				fail(w, http.StatusForbidden, "Origem não permitida.")
				return
			}
			if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				fail(w, http.StatusUnsupportedMediaType, "Envie application/json.")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func spa() http.Handler {
	dist, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(p, "api/") {
			fail(w, http.StatusNotFound, "Rota não encontrada.")
			return
		}
		if p != "" {
			if _, err := fs.Stat(dist, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "frontend não compilado: rode run.bat", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}
