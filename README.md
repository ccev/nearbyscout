# NearbyScout

Filters Golbat `nearby_stop` Pokemon webhooks and queues matching locations in Dragonite. `nearby_cell` routing is not implemented.

## Install

Requires Go 1.26. From this directory:

```sh
go build -o nearbyscout .
cp config.example.toml config.toml
# Edit config.toml before starting.
./nearbyscout
```

With no arguments, NearbyScout reads `config.toml` from the working directory. Use `-config PATH` to select another file. Local `config.toml` is ignored by Git.

Config groups: `[server]` controls listening, optional incoming auth and request limits; `[dragonite]` sets the scout endpoint, requester name (`username = "nearbyscout"`) and workers; `[queue]` bounds buffering and deduplication; `[filter]` contains an [Expr](https://expr-lang.org/) expression. Use `==`, not `===`.

The default matches IV 100%, Great or Ultra League rank <= 5, IV 0%, or species 201:

```text
iv == 100 || great_rank <= 5 || ultra_rank <= 5 || iv == 0 || pokemon_id == 201
```

Add to Golbat's configuration:

```toml
[[webhooks]]
url = "http://127.0.0.1:8080/webhook"
types = ["pokemon"]
# Optional: set server.token = "TOKEN" in NearbyScout too.
# headers = ["Authorization:Bearer TOKEN"]
```

Assumes an upcoming Golbat version supplies encounter-quality IVs, CP and ranks for `nearby_*`. Missing IVs do not match `iv == 0`. Dragonite outbound auth is intentionally absent. Delivery is in-memory and best effort, without retries; HTTP 202 means accepted, not successfully scouted. See [AGENTS.md](AGENTS.md) for the detailed contract.
