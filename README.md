# predbot

Go trading bot for cross-venue and intra-venue arbitrage on **Kalshi** and
**Polymarket US**. Implements the design in [`arbitrage-bot.md`](../../arbitrage-bot.md)
(v2, 2026-08-22).

## Status: Phase 1 (Record) done + running; Phase 2 (Backtest) scaffolded

- **Phase 1** (design §17): stream both venues, write raw + normalized frames,
  export metrics. Deployed on the sandbox VM in public-REST mode; see below.
- **Phase 2** scaffold: Book Manager, cross/intra-venue strategies (detect-only),
  fee/fill/replay models, and the §10.5 report. `cmd/backtest` replays the
  recorded data through the same pipeline with a SimExecutor. Runnable now.

Phases 3–5 (paper, live execution, freeze) are not built yet.

### Backtest (Phase 2)

```
go run ./cmd/backtest -data ~/backups/predbot/data
```

Deterministic single forward pass over the recorded `normalized/*.jsonl.gz`,
ordered by receive time (§10.1). Prints the §10.5 report: gross gap, fees per
venue, legging, net P&L, capital locked, pair counts. Models fill via book-walk
at t+Δ so fade is measured not modeled (§10.2), and treats a standing cross-venue
gap as one hold-to-resolution entry (§9, §10.4) rather than a per-tick trade.
Reports capital-weighted annualized return vs the idle-balance hurdle (§10.4),
Sharpe, win rate, and max drawdown (§10.5). **TODOs** (marked in code): Kalshi
fee accumulator (§6.1), independent-leg fill timing, streaming replay for
datasets larger than RAM.

### What's here

| Package | Role |
|---|---|
| `internal/core` | Domain model. Fixed-point `Price`/`Qty`, YES-centric `BookSnapshot`, quote derivation. Imports nothing (design §3). |
| `internal/venue/kalshi` | Delta-stream ingester with `seq` gap → snapshot **resync** (never drop, never reconnect on a gap). §7.1 |
| `internal/venue/polymarketus` | Full-snapshot ingester, **drop-on-backpressure** (next snapshot supersedes). §7.2 |
| `internal/journal` | Raw + normalized JSONL recorder, gzip, hourly rotation, fsync on rotate, SIGHUP-clean. §7.3 |
| `internal/obs` | OTel → homelab LGTM; the Phase-1 metric set. §13 |
| `internal/config` | `config.yaml` + `markets.yaml` loaders. Secrets come from env. §14 |
| `cmd/record` | Phase-1 entrypoint under systemd. |

Both ingesters normalize to a single YES instrument (§6.3): a Kalshi NO bid at
0.40 becomes a YES ask at 0.60; NO quotes are always derived, never stored.

### Frame-processing is pure and tested

`kalshi.State.ProcessFrame` and `polymarketus.ParseMarketData` are pure functions
over raw bytes, so the seq-gap/resync/normalization logic is unit-tested without
network or credentials (design §15 chaos tests): sequence gaps, delta-before-
snapshot, negative deltas that empty a level, truncated frames, hourly rotation.

```
go test -race ./...
```

## Build & run

```
go build ./cmd/record
./record -config config/config.yaml -markets config/markets.yaml           # authenticated WS (needs creds)
./record -public -config config/config.yaml -markets config/markets.yaml    # public REST, no creds (design §16 #5)
```

### Deploy

Runs as `predbot-record.service` on the amd64 k3s-sandbox VM (`root@192.168.3.172`)
in public-REST mode, shipping telemetry to the Pi's LGTM. Redeploy after a change:

```
scripts/redeploy.sh                 # test, cross-compile amd64, sync, restart, verify
scripts/redeploy.sh --skip-tests
```

Override target with env: `REMOTE=root@host SERVICE=predbot-record scripts/redeploy.sh`.

### Credentials (never in config — design §14)

Set in the environment (systemd `EnvironmentFile=/etc/predbot/env`, 0600):

- `KALSHI_API_KEY_ID`, `KALSHI_PRIVATE_KEY_PATH` — RSA key PEM; requests are
  RSASSA-PSS/SHA-256 signed.
- `POLYMARKET_US_API_KEY` — issued after iOS KYC (open item #5).

The dialers **refuse to connect** when credentials are missing rather than
connecting unauthenticated (both venues require auth even for public channels).

### Output

Under `data_dir`:

- `raw/<venue>-<YYYY-MM-DDThh>.jsonl.gz` — ground truth, one `{ts_recv_ns, venue,
  frame}` per line. Everything else regenerates from these.
- `normalized/<venue>-<YYYY-MM-DDThh>.jsonl.gz` — one `core.BookSnapshot` per line.

`SIGHUP` flushes/closes the current files (they reopen on next write);
`SIGINT`/`SIGTERM` shut down cleanly.

## ⚠️ `[VERIFY]` before Phase 4

Venue wire formats (field names, WS auth handshake, fixed-point conventions,
timestamp units) follow the design doc's verified-2026-08-22 facts and its
`[VERIFY]` markers. Re-check against live Kalshi/Polymarket US docs before wiring
real execution. See §16 open items.

## Jurisdiction (design §1)

This targets **Polymarket US** (`api.polymarket.us`, CFTC-regulated), **not**
Polymarket.com. Do not integrate `py-clob-client`, Polygon wallets, or CLOB
tokens — that is the wrong, US-geoblocked exchange.
