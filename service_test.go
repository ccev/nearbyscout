package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(t testing.TB) Config {
	t.Helper()
	c, err := loadConfig("config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testService(t testing.TB, c Config) *Service {
	t.Helper()
	s, err := newService(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.client.CloseIdleConnections)
	return s
}

func testEvent(id string, fields map[string]any) string {
	p := map[string]any{
		"encounter_id": id, "pokemon_id": 25, "seen_type": "nearby_stop",
		"latitude": 52.52, "longitude": 13.405,
		"individual_attack": 15, "individual_defense": 15, "individual_stamina": 15,
	}
	for k, v := range fields {
		p[k] = v
	}
	b, err := json.Marshal(map[string]any{"type": "pokemon", "message": p})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func testWebhook(t testing.TB, s *Service, body string, status int) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body)))
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, status, w.Body.String())
	}
}

func TestWebhookActivity(t *testing.T) {
	s := testService(t, testConfig(t))
	match := testEvent("match", nil)
	events := []string{
		match, match,
		testEvent("nonmatch", map[string]any{"individual_attack": 10}),
		testEvent("cell", map[string]any{"seen_type": "nearby_cell"}),
		testEvent("encounter", map[string]any{"seen_type": "encounter"}),
		`{"type":"raid","message":{}}`,
	}
	testWebhook(t, s, "["+strings.Join(events, ",")+"]", http.StatusAccepted)
	// Cross-request duplicates and rejected batch prefixes still represent activity.
	testWebhook(t, s, "["+match+"]", http.StatusAccepted)
	testWebhook(t, s, "["+match+",false]", http.StatusBadRequest)
	if s.received.Load() != 8 || s.matched.Load() != 4 {
		t.Fatalf("received=%d matched=%d, want 8/4", s.received.Load(), s.matched.Load())
	}
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	for _, counts := range [][2]uint64{{8, 4}, {0, 0}} {
		output.Reset()
		s.logActivity()
		var entry struct {
			Message  string `json:"msg"`
			Interval string `json:"interval"`
			Received uint64 `json:"received"`
			Matched  uint64 `json:"matched"`
		}
		if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Message != "webhook activity" || entry.Interval != "10s" || entry.Received != counts[0] || entry.Matched != counts[1] {
			t.Fatalf("unexpected activity log: %s", output.String())
		}
	}
	if s.accepted.Load() != 1 || s.duplicates.Load() != 2 || s.skippedCell.Load() != 1 || s.rejected.Load() != 1 {
		t.Fatal("activity logging changed cumulative counters")
	}
	testWebhook(t, s, "["+match+"]", http.StatusAccepted)
	if s.received.Load() != 1 || s.matched.Load() != 1 {
		t.Fatal("new interval did not start at zero")
	}
}

func TestWebhookActivityRejectedRequests(t *testing.T) {
	for _, reason := range []string{"auth", "slots", "queue"} {
		t.Run(reason, func(t *testing.T) {
			c := testConfig(t)
			c.Queue.Capacity = 1
			s := testService(t, c)
			status := http.StatusServiceUnavailable
			want := uint64(0)
			switch reason {
			case "auth":
				s.cfg.Server.Token = "secret"
				status = http.StatusUnauthorized
			case "slots":
				for range cap(s.slots) {
					s.slots <- struct{}{}
				}
			case "queue":
				s.queue <- scout{}
				want = 1
			}
			testWebhook(t, s, "["+testEvent("match", nil)+"]", status)
			if s.received.Load() != want || s.matched.Load() != want {
				t.Fatalf("received=%d matched=%d, want %d/%d", s.received.Load(), s.matched.Load(), want, want)
			}
		})
	}
}

func TestWebhookWholeArray(t *testing.T) {
	first, second := testEvent("first", nil), testEvent("second", nil)
	for _, body := range []string{
		"", "null", "{}", first, "[", "[" + first,
		"[" + first + ",]", "[" + first + ",false]", "[" + first + ",null]",
		"[" + first + `,{"type":"pokemon","message":{"pokemon_id":"bad"}}]`,
		"[" + first + `,{"type":"pokemon","message":]`,
		"[" + first + "]{}", "[" + first + "][]", "[" + first + "]null",
		"[" + first + "]garbage",
	} {
		t.Run(body, func(t *testing.T) {
			s := testService(t, testConfig(t))
			testWebhook(t, s, body, http.StatusBadRequest)
			if len(s.queue) != 0 || len(s.seen) != 0 || s.order.Len() != 0 || s.accepted.Load() != 0 {
				t.Fatal("invalid batch partially enqueued or entered dedup cache")
			}
			if s.rejected.Load() != 1 || len(s.slots) != 0 {
				t.Fatal("rejection not counted or request slot leaked")
			}
			testWebhook(t, s, "["+first+"]", http.StatusAccepted)
			if len(s.queue) != 1 {
				t.Fatal("rejected batch prevented a subsequent valid submission")
			}
		})
	}
	t.Run("complete array and trailing whitespace", func(t *testing.T) {
		s := testService(t, testConfig(t))
		testWebhook(t, s, "["+first+","+second+"] \n\t", http.StatusAccepted)
		if len(s.queue) != 2 || s.accepted.Load() != 2 {
			t.Fatal("did not process every array entry")
		}
		for _, id := range []string{"first", "second"} {
			if job := <-s.queue; job.ID != id {
				t.Fatalf("job = %+v, want ID %q", job, id)
			}
		}
		testWebhook(t, s, "[]", http.StatusAccepted)
	})
}

func TestWebhookTypesCoordinatesAndExpiry(t *testing.T) {
	s := testService(t, testConfig(t))
	events := []string{`{"type":"raid","message":{"pokemon_id":"ignored"}}`}
	for i, fields := range []map[string]any{
		{"seen_type": "nearby_cell"}, {"seen_type": "wild"}, {"seen_type": "encounter"}, {"seen_type": "lure_encounter"}, {"seen_type": ""},
		{"latitude": nil}, {"longitude": nil}, {"latitude": 90.01}, {"latitude": -90.01},
		{"longitude": 180.01}, {"longitude": -180.01}, {"encounter_id": ""}, {"pokemon_id": 0},
		{"disappear_time": time.Now().Add(-time.Hour).Unix()},
	} {
		events = append(events, testEvent(fmt.Sprint(i), fields))
	}
	// Omitted coordinates must be rejected just like explicit null coordinates.
	events = append(events, `{"type":"pokemon","message":{"encounter_id":"missing","pokemon_id":201,"seen_type":"nearby_stop"}}`)
	events = append(events, testEvent("origin", map[string]any{"latitude": 0, "longitude": 0}))
	events = append(events, testEvent("edge", map[string]any{"latitude": -90, "longitude": 180}))
	testWebhook(t, s, "["+strings.Join(events, ",")+"]", http.StatusAccepted)
	if len(s.queue) != 2 || s.skippedCell.Load() != 1 || s.expired.Load() != 1 {
		t.Fatalf("queue=%d, cells=%d, expired=%d", len(s.queue), s.skippedCell.Load(), s.expired.Load())
	}
	for _, want := range []scout{{ID: "origin", Location: [2]float64{0, 0}}, {ID: "edge", Location: [2]float64{-90, 180}}} {
		if got := <-s.queue; got != want {
			t.Fatalf("job = %+v, want %+v", got, want)
		}
	}
}

func TestWebhookFiltering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		match  bool
	}{
		{"perfect", nil, true},
		{"zero IV", map[string]any{"individual_attack": 0, "individual_defense": 0, "individual_stamina": 0}, true},
		{"null IV", map[string]any{"individual_attack": nil, "individual_defense": nil, "individual_stamina": nil}, false},
		{"partial IV", map[string]any{"individual_attack": nil, "individual_defense": 0, "individual_stamina": 0}, false},
		{"invalid IV summing to zero", map[string]any{"individual_attack": -1, "individual_defense": 1, "individual_stamina": 0}, false},
		{"invalid IV summing to perfect", map[string]any{"individual_attack": 16, "individual_defense": 14, "individual_stamina": 15}, false},
		{"ordinary", map[string]any{"individual_attack": 10}, false},
		{"species override", map[string]any{"pokemon_id": 201, "individual_attack": nil}, true},
		{"great later entry", map[string]any{"individual_attack": 10, "pvp": map[string]any{"great": []any{map[string]int{"rank": 50}, map[string]int{"rank": 5}, map[string]int{"rank": 100}}}}, true},
		{"ultra later entry", map[string]any{"individual_attack": 10, "pvp": map[string]any{"ultra": []any{map[string]int{"rank": 50}, map[string]int{"rank": 1}, map[string]int{"rank": 100}}}}, true},
		{"invalid and nonmatching ranks", map[string]any{"individual_attack": 10, "pvp": map[string]any{"great": []any{map[string]int{"rank": 0}, map[string]int{"rank": -1}, map[string]int{"rank": 6}}, "ultra": []any{map[string]int{"rank": 6}}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t, testConfig(t))
			testWebhook(t, s, "["+testEvent(tc.name, tc.fields)+"]", http.StatusAccepted)
			want := 0
			if tc.match {
				want = 1
			}
			if len(s.queue) != want || s.accepted.Load() != uint64(want) {
				t.Fatalf("queued %d, want %d", len(s.queue), want)
			}
		})
	}
	t.Run("omitted IVs are not zero", func(t *testing.T) {
		s := testService(t, testConfig(t))
		testWebhook(t, s, `[{"type":"pokemon","message":{"encounter_id":"missing-ivs","pokemon_id":25,"seen_type":"nearby_stop","latitude":52.52,"longitude":13.405}}]`, http.StatusAccepted)
		if len(s.queue) != 0 {
			t.Fatal("missing IVs matched as zero IVs")
		}
	})
}

func TestWebhookQueueOverloadAtomic(t *testing.T) {
	for _, prefill := range []bool{false, true} {
		t.Run(fmt.Sprint(prefill), func(t *testing.T) {
			c := testConfig(t)
			c.Queue.Capacity = 2
			s := testService(t, c)
			if prefill {
				testWebhook(t, s, "["+testEvent("existing", nil)+"]", http.StatusAccepted)
			}
			events := []string{testEvent("a", nil), testEvent("b", nil)}
			if !prefill {
				events = append(events, testEvent("c", nil))
			}
			testWebhook(t, s, "["+strings.Join(events, ",")+"]", http.StatusServiceUnavailable)
			want := 0
			if prefill {
				want = 1
			}
			if len(s.queue) != want || len(s.seen) != want || s.order.Len() != want || s.accepted.Load() != uint64(want) || s.rejected.Load() != 1 {
				t.Fatal("overloaded batch changed queue or dedup state")
			}
			if prefill {
				if job := <-s.queue; job.ID != "existing" {
					t.Fatalf("existing job replaced: %+v", job)
				}
			}
			testWebhook(t, s, "["+strings.Join(events[:2], ",")+"]", http.StatusAccepted)
			if len(s.queue) != 2 {
				t.Fatal("rejected IDs were incorrectly deduplicated on retry")
			}
		})
	}
}

func TestOptionalAuthentication(t *testing.T) {
	for _, token := range []string{"", "secret"} {
		for _, auth := range []string{"", "Bearer wrong", "secret", "bearer secret", "Bearer secret"} {
			for _, path := range []string{"/webhook", "/stats", "/healthz"} {
				t.Run(token+"/"+auth+path, func(t *testing.T) {
					c := testConfig(t)
					c.Server.Token = token
					s := testService(t, c)
					method, want := http.MethodGet, http.StatusOK
					if path == "/webhook" {
						method, want = http.MethodPost, http.StatusAccepted
					}
					if token != "" && auth != "Bearer "+token {
						want = http.StatusUnauthorized
					}
					if path == "/healthz" {
						want = http.StatusNoContent
					}
					r := httptest.NewRequest(method, path, strings.NewReader("["+testEvent("auth", nil)+"]"))
					r.Header.Set("Authorization", auth)
					w := httptest.NewRecorder()
					s.handler().ServeHTTP(w, r)
					if w.Code != want {
						t.Fatalf("status = %d, want %d", w.Code, want)
					}
					if want == http.StatusUnauthorized && (len(s.queue) != 0 || len(s.seen) != 0) {
						t.Fatal("unauthorized request changed state")
					}
				})
			}
		}
	}
}

func TestDeduplication(t *testing.T) {
	t.Run("within batch and across requests", func(t *testing.T) {
		s := testService(t, testConfig(t))
		event := testEvent("same", nil)
		before := time.Now()
		testWebhook(t, s, "["+event+","+event+"]", http.StatusAccepted)
		expires := s.seen["same"].Value.(dedupEntry).expires
		if expires.Before(before.Add(s.ttl)) || expires.After(time.Now().Add(s.ttl)) {
			t.Fatal("dedup expiry does not use configured TTL")
		}
		<-s.queue
		testWebhook(t, s, "["+event+"]", http.StatusAccepted)
		if len(s.queue) != 0 || s.duplicates.Load() != 2 || s.accepted.Load() != 1 {
			t.Fatal("duplicate was not suppressed")
		}
		if s.seen["same"].Value.(dedupEntry).expires != expires {
			t.Fatal("duplicate unexpectedly refreshed TTL")
		}
	})
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprintf("expired at capacity %d", capacity), func(t *testing.T) {
			c := testConfig(t)
			c.Queue.DedupCapacity = capacity
			s := testService(t, c)
			for i := 0; i < capacity; i++ {
				testWebhook(t, s, "["+testEvent(fmt.Sprint(i), nil)+"]", http.StatusAccepted)
				<-s.queue
			}
			// Expire the entry without timing-sensitive sleeps, including a full cap-1 cache.
			e := s.seen["0"]
			e.Value = dedupEntry{id: "0", expires: time.Now().Add(-time.Second)}
			testWebhook(t, s, "["+testEvent("0", nil)+"]", http.StatusAccepted)
			if len(s.queue) != 1 || len(s.seen) != capacity || s.order.Len() != capacity || s.duplicates.Load() != 0 {
				t.Fatal("expired entry was not safely replaced at capacity")
			}
			<-s.queue
			testWebhook(t, s, "["+testEvent("0", nil)+"]", http.StatusAccepted)
			if len(s.queue) != 0 || s.duplicates.Load() != 1 {
				t.Fatal("replacement did not start a new TTL")
			}
		})
	}
	t.Run("oldest eviction", func(t *testing.T) {
		c := testConfig(t)
		c.Queue.DedupCapacity = 2
		s := testService(t, c)
		for _, id := range []string{"a", "b", "c", "a"} {
			testWebhook(t, s, "["+testEvent(id, nil)+"]", http.StatusAccepted)
			if len(s.queue) != 1 {
				t.Fatalf("evicted ID %q was not accepted", id)
			}
			<-s.queue
			if id == "b" {
				// A duplicate hit must not turn FIFO eviction into LRU eviction.
				testWebhook(t, s, "["+testEvent("a", nil)+"]", http.StatusAccepted)
				if len(s.queue) != 0 {
					t.Fatal("active duplicate was not suppressed")
				}
			}
			if len(s.seen) > 2 || s.order.Len() != len(s.seen) {
				t.Fatal("dedup capacity exceeded or list diverged")
			}
		}
		if s.seen["b"] != nil || s.seen["c"] == nil || s.seen["a"] == nil {
			t.Fatal("wrong dedup entry evicted")
		}
	})
}

func TestDragoniteRequestAndStatuses(t *testing.T) {
	for _, status := range []int{200, 201, 202, 204, 299, 300, 302, 400, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/scout/v2" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected headers: %v", r.Header)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var got, want any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Error(err)
				}
				if err := json.Unmarshal([]byte(`{"username":"nearbyscout","options":{"pokemon":true,"gmf":true,"routes":false,"showcases":false},"locations":[[52.52,13.405],[-33.86,151.21]]}`), &want); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("body = %s, want exact Dragonite payload", body)
				}
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := testConfig(t)
			c.Server.Token = "incoming-secret-must-not-be-forwarded"
			c.Dragonite.Endpoint = server.URL + "/scout/v2"
			s := testService(t, c)
			s.send(context.Background(), []scout{
				{ID: "a", Location: [2]float64{52.52, 13.405}, Expires: time.Now().Add(time.Hour).Unix()},
				{ID: "expired", Location: [2]float64{1, 2}, Expires: time.Now().Add(-time.Hour).Unix()},
				{ID: "b", Location: [2]float64{-33.86, 151.21}},
			})
			if calls.Load() != 1 || s.expired.Load() != 1 {
				t.Fatalf("calls = %d, expired = %d; expected one attempt, no retries", calls.Load(), s.expired.Load())
			}
			wantSent, wantFailed := uint64(2), uint64(0)
			if status >= 300 {
				wantSent, wantFailed = 0, 2
			}
			if s.sent.Load() != wantSent || s.failed.Load() != wantFailed {
				t.Fatalf("sent=%d failed=%d, want %d/%d", s.sent.Load(), s.failed.Load(), wantSent, wantFailed)
			}
			s.send(context.Background(), []scout{{ID: "expired-only", Expires: time.Now().Add(-time.Hour).Unix()}})
			s.send(context.Background(), nil)
			if calls.Load() != 1 || s.expired.Load() != 2 || s.sent.Load() != wantSent || s.failed.Load() != wantFailed {
				t.Fatal("empty or fully expired batch attempted delivery or altered delivery counters")
			}
		})
	}
}

func BenchmarkWebhookLargeBatch(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := testService(b, testConfig(b))
			events := make([]string, count)
			for i := range events {
				events[i] = testEvent(fmt.Sprintf("%d", 9000000000000000000+int64(i)), map[string]any{
					"pokemon_id": 25, "individual_attack": 10, "individual_defense": 10, "individual_stamina": 10,
					"disappear_time": time.Now().Add(24 * time.Hour).Unix(), "cp": 650, "pokemon_level": 20,
					"form": 0, "costume": 0, "gender": 1, "weather": 1,
					"pokestop_id": "a-realistic-nearby-stop-id", "display_pokemon_id": 25,
					"pvp": map[string]any{"great": []any{map[string]int{"rank": 250, "cp": 1490}}, "ultra": []any{map[string]int{"rank": 900, "cp": 2200}}},
				})
			}
			body := []byte("[" + strings.Join(events, ",") + "]")
			h := s.handler()
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body)))
				if w.Code != http.StatusAccepted {
					b.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
			}
			b.StopTimer()
			if len(s.queue) != 0 || len(s.seen) != 0 || s.accepted.Load() != 0 || s.duplicates.Load() != 0 {
				b.Fatal("benchmark unexpectedly exercised queue or deduplication")
			}
		})
	}
}
