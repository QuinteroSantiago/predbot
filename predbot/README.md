# predbot

Go system for cross-venue arbitrage between **Kalshi** and **Polymarket US**
prediction markets. It records both venues' order books, backtests and
paper-trades on them, and runs a separate live trader against real money under a
risk engine nothing can bypass. It implements the design in
[`docs/design.md`](docs/design.md) (v2, 2026-08-22); `§` references in the code
point there.

## Status: retired (2026-10-14)

The project is finished and no longer runs. It answered its question: **the edge
isn't there at retail size.**

- **Live:** of 58 logged cross-venue pairs, only 1 hedged at its quoted prices.
  The Polymarket hedge quote was usually gone about 1 s after the Kalshi leg filled.
- **Taker backtest:** +$83 over the full ~83 GB archive, half of it from 3 games.
  Taker/taker arbitrage almost never clears the ~3.49¢ round-trip fee wall at
  retail depth.
- **Maker-rebate variant:** rest the Polymarket leg for the rebate, then hedge on
  Kalshi. Backtest adverse drift (0.287¢) sat just under the rebate (0.31¢), but
  the margin didn't survive live conditions.
- **Other models:** lead-lag, longshot-bias, and statistical variants were all
  dead on the recorded data.
- The account was wound down flat; no capital was lost.

Phases 1–4 of the design were built: record, backtest, paper, and live execution.
Phase 5 (freeze) was not.

The universe was discovered at runtime (`-sports`, MLB + NFL). `config/markets.yaml`
is `[]` on purpose: the static election/macro/crypto basket was removed on
2026-08-28 because its edges were phantom.

Deployment details below (hosts, paths, units) describe how it ran on a homelab;
host names like `predbot-vm` and `otel-collector` are placeholders.

## Layout

### Commands

| Command | Role | Runs where |
|---|---|---|
| `cmd/record` | Feed recorder: raw, normalized, and trade streams; `-paper` simulator; balance and edge gauges; `/healthz` on :9487 | VM, always on |
| `cmd/live` | **Real-money trader.** Taker (sequential Kalshi-lead → Poly-hedge) or `-maker`. Dry run by default. | VM, armed by hand |
| `cmd/preflight` | Read-only reconcile: balances, positions, open orders. Must pass before arming. | VM |
| `cmd/dumppos` | Read-only raw venue position dump (used by `trading.sh adopt-once`) | VM |
| `cmd/dumphist` | Read-only settlement and fill history, for reconciling balances against realized P&L | VM |
| `cmd/outcomes` | Records how markets resolved → `data/outcomes.jsonl` | VM |
| `cmd/backtest` | Replay, SimExecutor, and the §10.5 report. Also the `-maker`, `-adverse`, `-leadlag`, and `-longshot` analyzers and `-export` for the Fee Wall. | Archive host or off-box |
| `cmd/discover-sports` | Prints today's cross-listed games (`-league mlb\|nfl\|nba`) | anywhere |
| `cmd/testorder` | **Places real orders.** Previews, places, or cancels one order; used for path verification and manual flattening. | VM, by hand |
| `cmd/filltest` | **Can place real orders.** Private fill-stream probe. | VM, by hand |

### Packages

| Package | Role |
|---|---|
| `internal/core` | Domain model: fixed-point `Price`/`Qty` and a YES-centric `BookSnapshot`. Imports nothing (§3). |
| `internal/venue` | Ingester contract, WS dialer, trade-print streams (`trades.go`) |
| `internal/venue/kalshi` | Delta ingester. `seq` is one global counter per subscription, so a gap causes a reconnect and re-snapshot (§7.1). RSA-PSS auth. |
| `internal/venue/polymarketus` | Full-snapshot ingester, drop-on-backpressure (§7.2). Ed25519 auth. |
| `internal/venue/public` | Credential-free REST pollers (`record -public`, fallback only) |
| `internal/journal` | Gzip JSONL recorder with hourly rotation, fsync on rotate, SIGHUP-clean, and keep-markers (§7.3) |
| `internal/book` | Book Manager: canonical join across venues, `CrossEdge` with fillable size |
| `internal/strategy` | Pure decisions (§9): cross- and intra-venue, `MakerCrossVenue`, `MinDepth` gate |
| `internal/backtest` | Streaming replay, fee models, book-walk fill, resting-fill model, adverse selection, lead-lag, calibration, report |
| `internal/exec` | Executors: Kalshi Create-Order-V2, Polymarket US `/v1/orders`, `NopExecutor`, `PaperTrader` |
| `internal/risk` | The gate every order passes (`CheckAndReserve`) and the persisted `Kill()` |
| `internal/live` | Trader (taker and maker paths), private fill streams, venue reconcile, drift monitor, supervisor kill triggers |
| `internal/sports` | Daily game discovery and matching on both venues; MLB and NFL team tables; shared by record and live |
| `internal/outcome` | Settlement recording. Kalshi is native; Poly is derived from the co-listed Kalshi result. |
| `internal/obs` | OTel → homelab LGTM; every `predbot_*` metric (§13) |
| `internal/config` | `config.yaml` and `markets.yaml` loaders. Secrets come only from the environment (§14). |

Both ingesters normalize to a single YES instrument (§6.3). For example, a Kalshi NO
bid at 0.40 becomes a YES ask at 0.60. NO quotes are always derived, never stored.

## Build & test

```
go test -race ./...          # same as CI
go build ./cmd/record ./cmd/live ./cmd/backtest
```

The frame processors (`kalshi.State.ProcessFrame`, `polymarketus.ParseMarketData`)
are pure functions over raw bytes. Seq gaps, resync, normalization, and the
execution paths (legging, silent fills, drift, maker rest and hedge) are all
unit-tested without network or credentials.

CI (`.github/workflows/ci.yml` at the repo root) runs vet, `-race` tests, and an amd64 cross-compile
check. It **never deploys**. A push ships nothing.

## Deploy

Shipping code and arming real money are separate acts.

```
scripts/redeploy.sh [--skip-tests]   # RECORDER only → root@predbot-vm; refuses any other SERVICE
scripts/trading.sh deploy-live       # LIVE binaries + unit; refuses a dirty tree; never starts it
scripts/trading.sh start             # arm (runs preflight first)
```

`deploy-live` stamps the git short SHA into the binary. `predbot-live` logs it as
`LIVE MODE ARMED … build=<sha>`, so every trade traces back to a commit.

## Configuration & credentials

- `config/config.yaml` holds the venue URLs, `data_dir`, and risk caps. It also holds
  `backtest.min_depth` (1000 = 10 contracts), which is the recorder paper path's
  liquidity floor. The live trader's caps come from its unit flags, not this file.
- Secrets are only in the environment, via `EnvironmentFile=/etc/predbot/env`
  (0600 root:root) on the VM:
  - `KALSHI_API_KEY_ID`, `KALSHI_PRIVATE_KEY_PATH`: an RSA PEM
    (0640 root:predbot); requests are RSASSA-PSS/SHA-256 signed.
  - `POLYMARKET_US_KEY_ID`, `POLYMARKET_US_SECRET`: an Ed25519 seed, base64 or
    hex. Both venues sign the path **without** the query string.

The dialers refuse to connect when credentials are missing rather than connecting
unauthenticated.

## Data

Under `data_dir` (`/var/lib/predbot/data` on the VM):

- `raw/<venue>-<YYYY-MM-DDTHH>.jsonl.gz` is the ground truth: one
  `{ts_recv_ns, venue, frame}` per line.
- `normalized/<venue>-<YYYY-MM-DDTHH>.jsonl.gz` holds one `core.BookSnapshot` per
  line. It can be regenerated from raw, and `record -no-normalized` skips it.
- `trades/<venue>-trades-<YYYY-MM-DDTHH>.jsonl.gz` holds trade prints. These are
  the only way to measure adverse selection, because a book can't tell a trade
  from a cancel.
- `keep/<YYYY-MM-DDTHH>.keep` marks an hour (±1h) where any edge was at or
  above `-keep-edge-cents` (default 2¢). VM pruning ignores it; the archive
  host keeps every hour forever.
- `paper-fills.jsonl` is the paper trader's fills. `outcomes.jsonl` holds
  settlements.

An archive host pulls everything daily (`scripts/pull-backup.sh`) and keeps all of
it. `SIGHUP` flushes the current hour;
`SIGINT`/`SIGTERM` shut down cleanly.

## Backtest & analysis

```
go run ./cmd/backtest -data /var/lib/predbot/archive -days 3            # taker, §10.5 report
go run ./cmd/backtest -data … -maker -adverse                          # maker go/no-go
go run ./cmd/backtest -data … -leadlag | -longshot                     # read-only studies
```

Replay is a streaming k-way merge by receive time (§10.1), so memory stays bounded
on the full archive. Wall-clock time grows linearly with the archive: the full
archive took over 3h on a Raspberry Pi 5, so full-archive jobs ran on a 24-core
x86 box (`bt-amd64` build), never on the trading VM.

Open TODOs in code:
- The Kalshi per-order fee rounding accumulator (§6.1) is not modeled
  (`internal/backtest/fees.go:18`).
- `hourlyFiles` prefers a stale `normalized/` over a fresher `raw/`.
- Poly's native settled-outcome fields are unverified.

## Jurisdiction (design §1)

This targets **Polymarket US** (`api.polymarket.us` / `gateway.polymarket.us`,
CFTC-regulated), **not** Polymarket.com. Do not integrate `py-clob-client`, Polygon
wallets, or CLOB tokens. That is the wrong exchange, and it is geoblocked in the US.
