package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

type scout struct {
	ID       string
	Location [2]float64
	Expires  int64
}

type dedupEntry struct {
	id      string
	expires time.Time
}

type Service struct {
	cfg         Config
	filter      *vm.Program
	queue       chan scout
	slots       chan struct{}
	client      *http.Client
	ttl         time.Duration
	mu          sync.Mutex
	seen        map[string]*list.Element
	order       *list.List
	accepted    atomic.Uint64
	skippedCell atomic.Uint64
	duplicates  atomic.Uint64
	sent        atomic.Uint64
	failed      atomic.Uint64
	expired     atomic.Uint64
	rejected    atomic.Uint64
	received    atomic.Uint64
	matched     atomic.Uint64
}

func (s *Service) logActivity() {
	slog.Info("webhook activity", "interval", "10s", "received", s.received.Swap(0), "matched", s.matched.Swap(0))
}

func newService(c Config) (*Service, error) {
	program, err := compileFilter(c.Filter.Expression)
	if err != nil {
		return nil, err
	}
	timeout, _ := time.ParseDuration(c.Dragonite.Timeout)
	ttl, _ := time.ParseDuration(c.Queue.DedupTTL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = c.Dragonite.Workers
	return &Service{cfg: c, filter: program, queue: make(chan scout, c.Queue.Capacity),
		slots: make(chan struct{}, c.Server.MaxConcurrentRequests), ttl: ttl,
		seen: make(map[string]*list.Element), order: list.New(),
		client: &http.Client{Timeout: timeout, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (s *Service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", s.webhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"queue_depth": len(s.queue), "accepted": s.accepted.Load(),
			"nearby_cell_skipped": s.skippedCell.Load(), "duplicates": s.duplicates.Load(), "sent": s.sent.Load(),
			"failed": s.failed.Load(), "expired": s.expired.Load(), "rejected_requests": s.rejected.Load()})
	})
	return mux
}

func (s *Service) authorized(r *http.Request) bool {
	if s.cfg.Server.Token == "" {
		return true
	}
	want := sha256.Sum256([]byte("Bearer " + s.cfg.Server.Token))
	got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func (s *Service) webhook(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.rejected.Add(1)
		http.Error(w, "receiver busy", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	badJSON := func(err error) {
		s.rejected.Add(1)
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "expected Golbat event array", http.StatusBadRequest)
		}
	}
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		badJSON(err)
		return
	}
	// Stage only matches so malformed batches never cause partial delivery.
	var jobs []scout
	batchIDs := make(map[string]struct{})
	now := time.Now().Unix()
	for d.More() {
		var event *struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := d.Decode(&event); err != nil {
			badJSON(err)
			return
		}
		if event == nil {
			badJSON(nil)
			return
		}
		s.received.Add(1)
		if event.Type != "pokemon" {
			continue
		}
		var p Pokemon
		if err := json.Unmarshal(event.Message, &p); err != nil {
			badJSON(err)
			return
		}
		if p.SeenType == "nearby_cell" {
			// TODO: implement nearby_cell location selection in a separate change.
			s.skippedCell.Add(1)
			continue
		}
		if p.SeenType != "nearby_stop" {
			continue
		}
		if p.EncounterID == "" || p.PokemonID <= 0 || p.Latitude == nil || p.Longitude == nil || math.Abs(*p.Latitude) > 90 || math.Abs(*p.Longitude) > 180 {
			continue
		}
		if p.DisappearTime > 0 && p.DisappearTime <= now {
			s.expired.Add(1)
			continue
		}
		match, err := expr.Run(s.filter, p.env())
		if err != nil {
			s.rejected.Add(1)
			slog.Error("filter evaluation failed", "error", err)
			http.Error(w, "filter evaluation failed", http.StatusInternalServerError)
			return
		}
		if !match.(bool) {
			continue
		}
		s.matched.Add(1)
		if _, exists := batchIDs[p.EncounterID]; exists {
			s.duplicates.Add(1)
			continue
		}
		if len(jobs) == cap(s.queue) {
			s.rejected.Add(1)
			http.Error(w, "batch exceeds queue capacity", http.StatusServiceUnavailable)
			return
		}
		batchIDs[p.EncounterID] = struct{}{}
		jobs = append(jobs, scout{p.EncounterID, [2]float64{*p.Latitude, *p.Longitude}, p.DisappearTime})
	}
	if _, err := d.Token(); err != nil {
		badJSON(err)
		return
	}
	if _, err := d.Token(); err != io.EOF {
		badJSON(err)
		return
	}
	if !s.enqueue(jobs) {
		s.rejected.Add(1)
		http.Error(w, "scout queue full", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Service) enqueue(jobs []scout) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	unique := jobs[:0]
	for _, job := range jobs {
		if e := s.seen[job.ID]; e != nil && now.Before(e.Value.(dedupEntry).expires) {
			s.duplicates.Add(1)
			continue
		}
		unique = append(unique, job)
	}
	if len(unique) > cap(s.queue)-len(s.queue) {
		return false
	}
	for _, job := range unique {
		if old := s.seen[job.ID]; old != nil {
			s.order.Remove(old)
			delete(s.seen, job.ID)
		}
		if len(s.seen) >= s.cfg.Queue.DedupCapacity {
			old := s.order.Front()
			delete(s.seen, old.Value.(dedupEntry).id)
			s.order.Remove(old)
		}
		s.seen[job.ID] = s.order.PushBack(dedupEntry{job.ID, now.Add(s.ttl)})
		s.queue <- job
	}
	s.accepted.Add(uint64(len(unique)))
	return true
}

func (s *Service) worker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		var first scout
		var ok bool
		select {
		case <-ctx.Done():
			return
		case first, ok = <-s.queue:
			if !ok {
				return
			}
		}
		jobs := make([]scout, 0, s.cfg.Dragonite.BatchSize)
		jobs = append(jobs, first)
	collect:
		for len(jobs) < s.cfg.Dragonite.BatchSize {
			select {
			case <-ctx.Done():
				return
			case job, ok := <-s.queue:
				if !ok {
					break collect
				}
				jobs = append(jobs, job)
			default:
				break collect
			}
		}
		s.send(ctx, jobs)
	}
}

func (s *Service) send(ctx context.Context, jobs []scout) {
	locations := make([][2]float64, 0, len(jobs))
	now := time.Now().Unix()
	for _, job := range jobs {
		if job.Expires > 0 && job.Expires <= now {
			s.expired.Add(1)
			continue
		}
		locations = append(locations, job.Location)
	}
	if len(locations) == 0 {
		return
	}
	body, _ := json.Marshal(struct {
		Username  string          `json:"username"`
		Options   map[string]bool `json:"options"`
		Locations [][2]float64    `json:"locations"`
	}{s.cfg.Dragonite.Username, map[string]bool{"pokemon": true, "gmf": true, "routes": false, "showcases": false}, locations})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Dragonite.Endpoint, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		var resp *http.Response
		resp, err = s.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				err = errors.New(resp.Status)
			}
		}
	}
	if err != nil {
		s.failed.Add(uint64(len(locations)))
		slog.Error("scout submission failed; not retried", "locations", len(locations), "error", err)
		return
	}
	s.sent.Add(uint64(len(locations)))
}
