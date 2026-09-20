# Development Contract

## Scope and Sources

NearbyScout is a small Go 1.26 service connecting Golbat Pokemon webhook batches to Dragonite scout submissions. Keep changes minimal and preserve the bounded, best-effort pipeline. Do not add persistence, retries, outgoing authentication or nearby-cell routing without an explicit requirement.

The user expects an upcoming Golbat version, unavailable in the local checkout, to send encounter-quality IVs, CP and PvP ranks on `nearby_*` events. This is an integration assumption, not a capability verified against the current local Golbat. Do not reject nearby events merely because older Golbat usually lacks those fields, and never interpret absent IVs as zero IVs.

Reference sources inspected for this contract:

- `/home/malte/dev/golbat/decoder/pokemon_state.go`, `PokemonWebhook` and `createPokemonWebhooks` around line 379: nullable encounter fields, coordinates, `seen_type`, `pvp`, and string encounter IDs. The local webhook has no `cell_id`.
- `/home/malte/dev/golbat/webhooks/webhook.go`: both IV and non-IV Pokemon categories serialize as `type: "pokemon"`; config type `pokemon` subscribes to both. Payloads are JSON event arrays. The sender does not retry non-2xx responses and merely logs their status without treating that status as an error.
- `/home/malte/dev/golbat/config/config.go` and `reader.go`: `headers` is a list of `Name:Value` strings, not a TOML header table.
- `/home/malte/dev/dragonite/routes/scout.go` and `routes/main.go`: actual route is `POST /scout/v2`. The repository is `dragonite`, not `drago`.

## Architecture

- `main.go`: `-config` flag (default `config.toml`), startup, listener, fixed worker pool, signal handling and shutdown.
- `config.go`: TOML structures and strict loading with unknown fields rejected; validates endpoint, positive limits and durations.
- `filter.go`: webhook Pokemon model, nullable numeric values, typed Expr environment and startup compilation to a Boolean program.
- `service.go`: routes, optional incoming auth, streaming batch validation, filtering, bounded queue, encounter deduplication, batching, HTTP delivery and atomic counters.
- `config.example.toml`: tracked example settings, not implicit defaults. Copy to the Git-ignored `config.toml` for local use. With no arguments, the executable reads `config.toml` from its working directory; `-config PATH` overrides this. Configuration is read once at startup; there is no environment-variable override or live reload. Tests load the tracked example rather than local configuration.

## Inbound Contract

`POST /webhook` expects a JSON array of objects shaped as `{"type":"pokemon","message":{...}}`. Unknown event types are ignored. Unknown message fields are tolerated; known fields with incompatible JSON types reject the request.

- Process only `seen_type == "nearby_stop"`. Other seen types, including encountered Pokemon, do not trigger scouting.
- `nearby_cell` is counted and skipped before filtering. Location selection is a TODO: no routing or invented cell center, no assumption that local Golbat sends `cell_id`.
- Require a nonempty string `encounter_id`, positive `pokemon_id`, and non-null coordinates. Latitude must be within [-90, 90], longitude within [-180, 180]; zero coordinates are valid.
- `disappear_time` is Unix seconds. Positive timestamps at or before the current time are expired; absent, zero or negative values are treated as unknown expiry. Recheck expiry before sending.
- The decoder processes one event at a time, temporarily retaining its raw message. Stage only matching jobs and their encounter IDs, not the entire decoded input array.
- A request may stage at most `queue.capacity` distinct matching encounters, even if some would later be cache duplicates. Require the closing array and EOF; trailing data rejects the request.
- Commit only after the entire request validates. Under the enqueue mutex, remove active cache duplicates and check space for all remaining jobs before enqueuing any. Invalid or overloaded requests must not partially admit their own jobs or populate dedup entries. Workers can consume during admission; this is all-or-nothing admission, not atomic downstream execution.
- Counters for duplicates, skipped cells or expiry can advance while parsing a request that is later rejected; they are not transactionally rolled back.

Responses: 202 means parsing and queue admission succeeded, including batches with zero matches. It does not mean Dragonite accepted anything or a scout completed. Auth failure is 401; malformed payloads are 400; oversized bodies are 413; request-slot saturation, excessive staged matches or insufficient queue space are 503; filter runtime errors are 500. Local Golbat does not retry non-2xx failures, so rejection may lose events.

`GET /healthz` returns unauthenticated 204 and is only a liveness check. `GET /stats` uses the same optional auth as `/webhook` and reports `queue_depth`, `accepted`, `nearby_cell_skipped`, `duplicates`, `sent`, `failed`, `expired`, and `rejected_requests`. These are process-local observations, not a delivery ledger; auth failures are not included in `rejected_requests`.

## Filter Environment

Compile `filter.expression` once at startup with the typed environment and a required Boolean result. Use Expr operators (`==`, `&&`, `||`, parentheses, `in`), not JavaScript's `===`. Evaluate the shared compiled program per eligible event without compiling per event.

All exposed fields:

| Field | Type and meaning |
| --- | --- |
| `pokemon_id` | Integer species ID; must be positive before evaluation |
| `seen_type` | String; currently only `nearby_stop` reaches evaluation |
| `iv` | Float percentage `(attack + defense + stamina) * 100 / 45`, or -1 if unknown/invalid |
| `iv_known` | Boolean; all three IV components are present and within 0..15 |
| `individual_attack` | Integer attack IV; missing/null is -1 |
| `individual_defense` | Integer defense IV; missing/null is -1 |
| `individual_stamina` | Integer stamina IV; missing/null is -1 |
| `cp` | Integer CP; missing/null is -1 |
| `pokemon_level` | Float level; missing/null is -1 |
| `form` | Integer form; missing/null is -1 |
| `costume` | Integer costume; missing/null is -1 |
| `gender` | Integer gender; missing/null is -1 |
| `weather` | Integer weather; missing/null is -1 |
| `great_rank` | Minimum positive rank in `pvp.great`, or 2147483647 |
| `ultra_rank` | Minimum positive rank in `pvp.ultra`, or 2147483647 |

Nullable numeric fields use pointers in the wire model to preserve missing versus actual zero. Explicit out-of-range IV components remain visible as supplied but make `iv_known` false and `iv` -1. Missing IVs must never match `iv == 0`. For other threshold filters, remember that -1 satisfies expressions such as `cp < 100`; add a known-value guard where needed.

PvP leagues are arrays of objects containing integer `rank`. Reduce across every supplied entry, including all evolutions and level caps, not just the first entry or base species. Ignore zero and negative ranks; absent/null/empty leagues or no usable ranks leave the sentinel 2147483647. Do not derive ranks locally or filter entries by evolution/cap.

The shipped expression is `iv == 100 || great_rank <= 5 || ultra_rank <= 5 || iv == 0 || pokemon_id == 201`. Species 201 may match without IVs. Coordinates, encounter ID, disappearance time and the raw `pvp` map are not exposed to Expr.

## Configuration

| Key | Shipped value | Purpose |
| --- | --- | --- |
| `server.host` | `127.0.0.1` | Listener host |
| `server.port` | `8080` | TCP port, 1..65535 |
| `server.token` | empty | Optional incoming Bearer token |
| `server.max_body_bytes` | `16777216` | Maximum webhook body bytes |
| `server.max_concurrent_requests` | `8` | Concurrent webhook processing slots |
| `dragonite.endpoint` | `http://127.0.0.1:7272/scout/v2` | Full outbound URL |
| `dragonite.username` | `nearbyscout` | Scout requester name, sent as `username` |
| `dragonite.workers` | `2` | Fixed outbound worker count |
| `dragonite.batch_size` | `100` | Maximum locations per outbound request |
| `dragonite.timeout` | `5s` | Outbound HTTP timeout |
| `queue.capacity` | `10000` | Buffered scout jobs and per-request staging limit |
| `queue.dedup_capacity` | `100000` | Maximum retained encounter IDs |
| `queue.dedup_ttl` | `10m` | Suppression interval from admission |
| `filter.expression` | See above | Nonempty Boolean Expr expression |

Limits must be positive; durations use Go duration syntax and must be positive. Endpoint validation requires HTTP(S), a host, no embedded credentials and no fragment. Host, username and token may be empty. No outbound token field exists: the user intentionally removed it; do not restore it as compatibility behavior.

Use `net.JoinHostPort` for the listener. IPv4 and IPv6 are supported by the address construction: put an unbracketed IPv6 literal such as `::1` in `server.host`, but bracket IPv6 literals in URLs, e.g. `http://[::1]:7272/scout/v2`. An empty listener host binds wildcard addresses; actual IPv4/IPv6 behavior depends on the OS. Loopback URLs work only within the same host/network namespace; configure reachable addresses for separate containers or hosts.

An empty `server.token` disables incoming authentication. Otherwise `/webhook` and `/stats` require exactly `Authorization: Bearer TOKEN`; the implementation compares SHA-256 hashes in constant time. Configure Golbat with `headers = ["Authorization:Bearer TOKEN"]`. `/healthz` stays public. The listener is plain HTTP; use trusted networking or a TLS reverse proxy and do not commit real tokens.

## Queue and Delivery

The design targets workloads on the order of one million events/hour, not a measured throughput guarantee. Preserve bounded active webhook concurrency, body size, per-request match staging, queue capacity, cache capacity and worker count. Do not introduce a goroutine per event or an unbounded backlog. Total staging can scale with concurrent requests times queue capacity; a streaming decoder is not constant-memory processing of an individual event.

Deduplicate by encounter ID, not species or location, within each request and across accepted requests. The cache uses TTL checks plus FIFO capacity eviction, not LRU. Duplicate hits do not refresh TTL. Expired entries can remain allocated until replaced or evicted, but the cache is bounded. Capacity eviction can permit a repeat before TTL, so suppression is best effort, not exactly-once delivery. Admission records the ID before delivery; expiry or downstream failure does not remove it.

Workers block for the first job, then immediately collect available jobs up to `batch_size` without waiting to fill a batch. Different encounters at the same location are not merged. Multiple workers can reorder downstream submissions.

Dragonite receives `POST` to the configured endpoint with `Content-Type: application/json`, no outgoing Authorization header, and this body shape:

```json
{
  "username": "nearbyscout",
  "options": {"pokemon": true, "gmf": true, "routes": false, "showcases": false},
  "locations": [[51.5, -0.12]]
}
```

Coordinate order is always `[latitude, longitude]`. Do not use the deprecated bare-array scout endpoint. Reuse the bounded worker pool and shared HTTP client/transport, enforce the configured timeout, and never follow outgoing redirects. A redirect is a failed submission, not a new destination. The response body is discarded up to 4096 bytes and closed.

Any 2xx counts submitted locations as `sent`; transport errors and non-2xx count them as `failed`. Dragonite's acknowledgement means queued there, not completed scouting. There is no completion callback, response-body success interpretation, retry, durable queue or crash recovery. Failed and expired jobs are dropped. Restart loses queued jobs, dedup state and counters.

## Lifecycle

Startup must fail for invalid config, invalid/non-Boolean filters or listener errors. HTTP limits currently include a 5s header timeout, 30s read/write timeouts, 60s idle timeout and 16 KiB maximum headers.

SIGINT/SIGTERM starts a shared 30-second shutdown budget: stop HTTP admission and wait for handlers, then close the queue and drain workers. If HTTP shutdown times out, close connections and cancel workers without closing a queue handlers might still write to. At the deadline cancel outbound requests and discard remaining work. Preserve this bounded-shutdown design rather than draining indefinitely; the budget is not a promise of durable delivery.

## Commands and Verification

Run from `/home/malte/dev/nearbyscout` with Go 1.26:

```sh
go build -o nearbyscout .
./nearbyscout
go vet ./...
go test ./...
go test -race ./...
go test -run '^$' -bench . -benchmem ./...
```

Tests in `config_test.go`, `filter_test.go`, `service_test.go`, and `concurrency_test.go` cover configuration, filtering, request validation/admission, deduplication, outbound delivery, concurrent workers, draining, cancellation and timeouts. `BenchmarkWebhookLargeBatch` measures single-request parsing/filtering of 1,000- and 10,000-event nonmatching batches with allocations reported; it does not measure matching admission or downstream scouting. Full process shutdown under overloaded HTTP traffic is not covered by these tests. Report which checks actually ran, and do not claim million-events/hour capacity from compilation or a synthetic microbenchmark.

For behavioral changes, prioritize tests for missing/null/zero/invalid IVs, minimum positive ranks across evolutions/caps, Expr type checking, nearby-stop eligibility, skipped nearby-cell events, coordinate bounds, expiry at ingress and delivery, auth, malformed/trailing JSON, body/concurrency/queue limits, atomic admission, TTL/FIFO eviction, outbound payload shape, no redirects/auth/retries, downstream errors and bounded shutdown. Use local HTTP test servers rather than live scouting. Run race checks for queue/cache and shutdown changes. Benchmark realistic mixed batches with match rates, payload sizes, concurrency and downstream latency recorded; distinguish parsing throughput, admission throughput and actual scout completion.
