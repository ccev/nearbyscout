package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestLimits(t *testing.T) {
	body := "[" + testEvent("limited", nil) + "]"
	for _, extra := range []string{"", strings.Repeat(" ", 100)} {
		c := testConfig(t)
		c.Server.MaxBodyBytes = int64(len(body) - 1)
		s := testService(t, c)
		testWebhook(t, s, body+extra, http.StatusRequestEntityTooLarge)
		if len(s.queue) != 0 || len(s.seen) != 0 || len(s.slots) != 0 {
			t.Fatal("oversized request changed state or leaked a slot")
		}
	}
	c := testConfig(t)
	c.Server.MaxBodyBytes = int64(len(body))
	s := testService(t, c)
	testWebhook(t, s, body, http.StatusAccepted)
	testWebhook(t, s, body+" ", http.StatusRequestEntityTooLarge)
	for range cap(s.slots) {
		s.slots <- struct{}{}
	}
	testWebhook(t, s, "[]", http.StatusServiceUnavailable)
	for range cap(s.slots) {
		<-s.slots
	}
	testWebhook(t, s, "[]", http.StatusAccepted)
}

func TestConcurrentAdmissionAndWorkers(t *testing.T) {
	var received atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Locations [][2]float64 `json:"locations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		received.Add(int64(len(payload.Locations)))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	c := testConfig(t)
	c.Dragonite.Endpoint = server.URL
	c.Server.MaxConcurrentRequests = 64
	s := testService(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers, requests sync.WaitGroup
	for range c.Dragonite.Workers {
		workers.Go(func() { s.worker(ctx) })
	}
	body := "[" + testEvent("same-encounter", nil) + "]"
	h := s.handler()
	for range 64 {
		requests.Go(func() {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body)))
			if w.Code != http.StatusAccepted {
				t.Errorf("status = %d", w.Code)
			}
		})
	}
	requests.Wait()
	if s.received.Load() != 64 || s.matched.Load() != 64 {
		t.Fatalf("activity counters lost events: received=%d matched=%d", s.received.Load(), s.matched.Load())
	}
	close(s.queue)
	workers.Wait()
	if received.Load() != 1 || s.accepted.Load() != 1 || s.sent.Load() != 1 || s.duplicates.Load() != 63 {
		t.Fatalf("received=%d accepted=%d sent=%d duplicates=%d", received.Load(), s.accepted.Load(), s.sent.Load(), s.duplicates.Load())
	}
}

func TestWorkerBatchingAndDrain(t *testing.T) {
	sizes := make(chan int, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Locations [][2]float64 `json:"locations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		sizes <- len(payload.Locations)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	c := testConfig(t)
	c.Dragonite.Endpoint = server.URL
	c.Dragonite.BatchSize = 2
	s := testService(t, c)
	for range 5 {
		s.queue <- scout{Location: [2]float64{1, 2}}
	}
	close(s.queue)
	s.worker(context.Background())
	if len(sizes) != 3 || s.sent.Load() != 5 {
		t.Fatalf("batch count = %d, sent = %d", len(sizes), s.sent.Load())
	}
	for _, want := range []int{2, 2, 1} {
		if got := <-sizes; got != want {
			t.Fatalf("batch size = %d, want %d", got, want)
		}
	}
}

func TestWorkerCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancellation", true: "timeout"}[timeout], func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			c := testConfig(t)
			c.Dragonite.Endpoint = server.URL
			if timeout {
				c.Dragonite.Timeout = "100ms"
			}
			s := testService(t, c)
			s.queue <- scout{Location: [2]float64{1, 2}}
			close(s.queue)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { s.worker(ctx); close(done) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not submit")
			}
			if !timeout {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not stop")
			}
			if s.failed.Load() != 1 || s.sent.Load() != 0 {
				t.Fatal("failed request not accounted for")
			}
		})
	}
}
