# NearbyScout

Filters Golbat `nearby_stop` Pokemon webhooks and queues matching locations in Dragonite. `nearby_cell` routing is not yet implemented.

## Install

Requires Go 1.26

```sh
go build
cp config.example.toml config.toml
# Edit config.toml before starting.
./nearbyscout
```

Add to your Golbat config:

```toml
[[webhooks]]
url = "http://127.0.0.1:7733/webhook"
types = ["pokemon"]
# Optional: Set server.token = "TOKEN" in nearbyscout
# headers = ["Authorization:Bearer TOKEN"]
```

## Config

With no arguments, NearbyScout reads `config.toml` from the working directory. Use `-config PATH` to select another file.

Use [Expr](https://expr-lang.org/) to configure your filters. Default will match 100% and 0% IVs, Great and Ultra League Ranks >= 5, and Unown.

```text
iv == 100 || great_rank <= 5 || ultra_rank <= 5 || iv == 0 || pokemon_id == 201
```
