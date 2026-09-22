# Backtest parallelism: parallel decode + shard the sim by market

**Status:** implemented on branch `backtest-shard-sim`; validation on real data
on the arch box still to do (see "Results")
**Date:** 2026-09-22
**Scope:** `cmd/backtest`, `internal/backtest`
**Related:** design §10.1 (replay), §10.2 (latency Δ), §10.4 (capital/return)

## Problem

A full `-maker` backtest pins **one core** and leaves the rest idle. Measured on
the arch compute box (24c/30GB): `backtest` at ~108% CPU, load average ~1.0,
95% of the machine idle, while a full-archive pass runs for tens of minutes.

The original version of this doc blamed the single sim consumer alone. That
was half right. Profiling during implementation showed **two** serial stages:

1. **Decode was effectively serial.** `streamHours` starts up to 16 hour
   decoders, but each look-ahead hour may run only `batchesAhead` (2) batches,
   i.e. 2,048 snapshots, ahead of the consumer before it blocks. An hour holds
   hundreds of thousands of snapshots, so in practice one hour decoded at a
   time. JSON decoding alone was ~60% of CPU on a synthetic `-maker` run, and
   gunzip + line scanning another ~14%.
2. **The sim consumer is serial by design.** On the same run it was ~15% of
   CPU. Once decode is parallel it becomes the ceiling on a many-core box.

Both are fixed below. Sharding the sim alone gave no speedup at all, because
decode was the bigger serial stage.

## Key insight: markets are independent

The per-tick replay body (`cmd/backtest/main.go`, the second `StreamReplay`) has
**no cross-market coupling**. Everything mutable is keyed by `canonical`:

- book state: `bm.Update(s)`, `bm.Pair(canonical)`
- entry/episode gates: `entered[canonical]`, `active[canonical]`
- maker resting state: `resting[canonical]`, `restTaking[canonical]`
- spread sampling: `lastBucket[canonical]`
- sim accounting: `Sim.pending` (each `pendingPair` carries its `canonical`),
  `Report.byCanonical`, `Report.pairs`

Crucially there is **no global capital/budget gate**. `Report.CapitalLocked`
(sim.go:112, :249) is an after-the-fact receipt, not a constraint that throttles
entries in another market. So market A's fills never affect market B's outcome.

`Sim.settle` (sim.go:67) prices a pair only against that market's book
(`bm.Pair(p.canonical)`); `Sim.Advance` uses `now` solely as the `p.at > now`
fill-time gate. A market's book mutates **only** on that market's own snapshots,
and `Advance` runs *before* `Update` each tick. Therefore whichever snapshot
first crosses `p.at` settles against the identical book state — the result does
not depend on how snapshots from *other* markets are interleaved.

That makes sharding by market **correct**, not merely fast.

## Design

Keep the parallel decode. Replace the single sim consumer with **N sim shards,
one goroutine each**, routing every snapshot to a fixed shard chosen by
`canonical`. A market's snapshots always land on the same shard, so per-market
time order is preserved — the only ordering the sim requires.

```
                       ┌─ shard 0: own bm, sim, report, per-market maps
StreamReplay (decode,  │
  ≤16 workers, ordered)─┤─ router: canonical → FNV%N → shard's batch channel
   → single callback    │
                       └─ shard N-1: …
                          (N ≈ NumCPU, PREDBOT_SIM_SHARDS)
        end of stream → close channels → each shard sim.Flush → MERGE → Finalize
```

### 1. Extract the per-tick body into a shard

Lift `main.go`'s second-pass closure (the `Advance → Update → Pair → strategy →
resting/submit` body) into a method on a struct that owns the state currently
captured by the closure:

```go
type shard struct {
    bm         *book.Manager        // seeded with the same markets; only ITS
                                     // markets' books ever mutate here
    sim        *Sim                 // own pending + own *Report
    rep        *Report
    entered    map[string]bool
    active     map[string]bool
    resting    map[string]*RestingFill
    restTaking map[string]core.Intent
    lastBucket map[string]int64
    opps       []Opportunity
    spread     []SpreadPoint
    restsPlaced, restsFilled int

    // read-only, shared across shards:
    fees       Fees
    scfg       strategy.Config
    maxAge, maxSkew int64
    resolution map[string]time.Time
    maker, spreadOn bool
}

func (sh *shard) tick(s core.BookSnapshot) // verbatim today's loop body
```

### 2. Fan-out harness (replaces the second `StreamReplay` call)

- `N := simShards()` = `min(runtime.NumCPU(), cap)`, overridable via
  `PREDBOT_SIM_SHARDS`. Independent of the decode `maxWorkers` pool. Default ~20
  on the arch box.
- Reuse the `bm` already built at main.go:97–102 as a **read-only canonical
  lookup** for routing (`Canonical()` only — never `Update`).
- Each shard gets its own `book.Manager` (same market set), `NewSim`/`NewReport`,
  and a buffered `chan []core.BookSnapshot`.
- The `StreamReplay` callback becomes a thin **router**: look up `canonical`;
  `idx := fnv32a(canonical) % N`; append to `idx`'s batch; flush the batch (size
  ~1024, mirroring `stream.batchSize`) to the shard channel when full. Snapshots
  with no canonical are dropped, exactly as today.
- Each shard goroutine drains its channel, calls `sh.tick` per snapshot in FIFO
  order; on channel close it calls `sh.sim.Flush(sh.bm)` and returns.

The router is cheap (one map lookup + a slice append), so it keeps up with the
decode; the heavy work (`bm.Update`, strategy eval, sim) now runs across N cores.

### 3. Merge (after all shards finish, before `Finalize`)

Markets are disjoint across shards, so the merge has no key collisions:

- **Sum** scalars: `GrossCaptured, Slippage, LeggingCost, NetPnL, CapitalLocked,
  PairsAttempted, PairsCompleted, PairsLegged, PairsFaded, IntraCrossings,
  restsPlaced, restsFilled`.
- **Merge maps:** `FeesByVenue` (sum per venue), `byCanonical` (union), `pairs`
  (concat), `opps`/`spread` (concat).
- **Sort merged `pairs` by `entryTs`** (tie-break `canonical`). This is the one
  ordering that matters: `Report.Finalize` (report.go:69) walks `pairs` in order
  to compute the cumulative-equity curve and `MaxDrawdown`. Sharpe, WinRate,
  Annualized, AvgHoldDays are order-independent (sums / mean-variance).
- For deterministic export files, sort `opps`/`spread` by timestamp.
- Call `rep.Finalize(resolution, idleYield)` **once** on the merged report, then
  `String()`.

## Correctness / acceptance test

The economic argument above says results are **byte-identical** to serial (after
the `entryTs` sort). Prove it — do not assume it. The settlement *trigger* fires
on the first same-shard snapshot past `p.at` rather than the first global one; the
claim is that this is economically identical because a market's book is constant
between its own snapshots.

Extend the existing `internal/backtest/stream_parallel_test.go` fixture harness
with a **report-equality test**:

- Run the trading replay at `shards ∈ {1, 2, 4, 8}` against a fixed multi-market
  fixture and compare the finalized `Report` (all scalar fields + `FeesByVenue`
  + `byCanonical` + `pairs` sorted by `entryTs`).
- `shards == 1` must equal the current serial path **exactly**.
- `shards > 1` must equal serial after the merge sort.

This test is the acceptance gate; it also guards against a future strategy change
accidentally introducing cross-market state.

## Scope

**In scope (the slow paths):** `-maker` and the default cross-venue trading path.

**Left serial (already decode-bound / read-only):** `-leadlag`, `-longshot`, and
the first id-scan pass.

**`-adverse` stays serial for v1.** Its `advTrades`/`advIdx` is a single global
time-ordered trade stream consumed across all markets. Sharding it means routing
trade prints to shards by `canonical` too (they carry venue/venueID) — a clean
follow-up, not v1. Fall back to a single shard when `-adverse` is set.

## Load balancing

Start with `fnv32a(canonical) % N`. Market snapshot counts are skewed, so if one
shard lags, use the per-canonical counts already available from the id-scan pass
to greedily bin-pack markets onto shards (longest-processing-time first). Build
this only if the hash proves uneven in practice.

## Expected result

Near-linear speedup in shard count until the decode side (≤16 workers) becomes
the new floor — realistically ~10–16× on the 24-core box. Once the sim is no
longer the bottleneck, revisit the `maxWorkers=16` decode cap (raise or make it
`NumCPU`-derived) to push further.

## Effort

~150–250 lines across `cmd/backtest/main.go` and `internal/backtest`, plus the
equality test. No changes to the recorder, strategy, or on-disk data format.

## Implementation plan

Five steps, each a separately mergeable commit that leaves `backtest` working
and its output unchanged until step 5 flips the default.

**Baseline first.** Before touching code, capture a golden output on the arch
box so every step can be diffed against it:

```
./backtest -data ~/software/predbot/data -maker -days 3 > golden-maker-3d.txt
./backtest -data ~/software/predbot/data        -days 3 > golden-taker-3d.txt
```

`-days 3` keeps each check to minutes while still covering many markets and
real fills.

### Step 1 — Move the trading replay into `internal/backtest` (pure refactor)

Why: the equality test (step 4) needs the fixture helpers in
`internal/backtest/stream_parallel_test.go`, which a test in `package main`
can't reach. The import graph allows it: `internal/backtest` already imports
`book` and `strategy`, and `strategy` imports only `core`, so there's no cycle.

- New `internal/backtest/trading.go`:
  - `type TradingOpts struct` — `Fees`, `LatencyMS`, `Strategy strategy.Config`,
    `MaxAge`, `MaxSkew`, `Resolution map[string]time.Time`, `Maker`, `Spread bool`.
  - `type TradingResult struct` — `Report *Report`, `Opps []Opportunity`,
    `Spread []SpreadPoint`, `RestsPlaced`, `RestsFilled int`.
  - `type shard struct` holding the state listed under Design §1, with
    `newShard(markets []config.Market, opts TradingOpts) *shard` (its own
    `book.NewManager(markets)`, `NewReport`, `NewSim`, empty maps).
  - `func (sh *shard) tick(s core.BookSnapshot)`: the body of main.go's second
    `StreamReplay` closure, moved verbatim with `sh.` prefixes.
  - `func (sh *shard) finish()`: calls `sh.sim.Flush(sh.bm)`.
  - `func RunTrading(dir string, window int, markets []config.Market, opts TradingOpts) (*TradingResult, error)`:
    for now it builds one shard and drives it from `StreamReplay`.
- Move `staleAny` and `skewExceeds` from main.go into `trading.go` (unexported).
- `main.go`: build `markets` = markets.yaml + reconstructed sports markets (the
  same slice currently passed to `book.NewManager`/`AddMarkets`) and replace the
  second pass (main.go:165–304) with one `RunTrading` call. `-adverse` observes
  books inside the same per-tick body, so it rides along as an optional hook in
  `TradingOpts` (`Adverse`, `Trades`) and the shard owns the trade cursor. At
  one shard that is exactly today's behaviour; step 3 pins adverse runs to one
  shard because the cursor is global across markets.
- The call to `rep.Finalize` and all printing stay in main.go, reading from
  `TradingResult`.

**Verify:** `go build ./... && go test ./...`; on the arch box, diff both runs
against the golden files. They must be identical.

*Done 2026-09-22 (branch `backtest-shard-sim`).* Verified on the Pi by diffing
the old and new binaries against a synthetic archive (8 markets, 292k snapshots,
3h, crossing gaps, 5,860 trade prints): output was identical for the default,
`-maker`, `-maker -adverse`, `-export`/`-export-spread` files, and `-hours 1`.
The golden diff against real data on the arch box is still to do.

### Step 2 — `Report.Merge` and ordered finalize

- `internal/backtest/report.go`: `func (r *Report) Merge(o *Report)` sums the
  scalar counters, sums `FeesByVenue` per venue, unions `byCanonical`, and
  appends `pairs`. It does **not** touch the derived fields; those belong to
  `Finalize`.
- Add a sort at the top of `Finalize`: `pairs` by `(entryTs, canonical)`,
  stable. Serial order is already entry order, so today's output doesn't change,
  and merged reports come out deterministic.
- Unit test (`report_test.go`): build two reports by hand with disjoint
  canonicals and interleaved `entryTs`. Assert that `Merge` then `Finalize`
  equals one report fed the same pairs serially. That covers `MaxDrawdown`,
  which is the order-sensitive stat.

**Verify:** unit tests pass; golden diff is still identical.

### Step 3 — Fan-out harness (default still 1 shard)

- `internal/backtest/trading.go`:
  - `func simShards() int`: `runtime.NumCPU()`, overridden by
    `PREDBOT_SIM_SHARDS`, clamped to at least 1. **Returns 1 unless the env var
    is set** in this step.
  - `RunTrading` with N > 1:
    - Build a routing `book.Manager` from the same markets and only ever call
      `Canonical()` on it.
    - Create N shards, each with a `chan []core.BookSnapshot` of capacity ~4 and
      a `sync.WaitGroup`.
    - Router, inside the `StreamReplay` callback: canonical lookup, then
      `fnv32a(canonical) % N`, then append to that shard's pending batch. Send the
      batch when it reaches 1024 and start a fresh slice; don't reuse the backing
      array, since the shard still owns it. Drop snapshots that have no
      canonical, as main.go does today.
    - Shard goroutine: `for batch := range ch { for _, s := range batch { sh.tick(s) } }; sh.finish()`.
    - At end of stream, flush any partial batches, close every channel, then
      `wg.Wait()`.
    - Merge: `Report.Merge` over all shards; concat `Opps`/`Spread` and sort each
      by timestamp then canonical; sum the rest counters.
  - Errors: `StreamReplay` errors propagate after channels are closed and
    workers drained. That way a decode failure leaks no goroutines, matching the
    guarantee `stream.go` already makes.
- `main.go`: force N=1 when `-adverse` is set, and add a one-line note saying why
  (it has a global trade cursor; see Scope).

**Verify:** default run is identical to golden. `PREDBOT_SIM_SHARDS=8` should be
identical too, except possibly for `-export` file order, which the sort handles.
Check `htop` to confirm more than one core is busy.

### Step 4 — Equality test (the acceptance gate)

- New fixture helper `writeTradingArchive(t, hours, markets)`. The existing
  `writeSyntheticArchive` can't be used: its books never cross (bids 40–44, asks
  60–64 on both venues), so no edge ever opens and an equality test would pass
  with zero trades. The new fixture needs:
  - about 12 canonical markets, each listed on both venues, with matching
    `[]config.Market` entries so `book.Manager` can pair them;
  - per-market price paths that open and close cross-venue gaps above `MinEdge`
    at different times, so markets are entered in an interleaved order;
  - some books that later trade *through* a resting maker price, so rests fill,
    and some that don't;
  - at least one gap that vanishes within the latency Δ, which exercises
    `PairsFaded`.
- `trading_parallel_test.go`: for `maker ∈ {false, true}` and
  `shards ∈ {1, 2, 4, 8, 16}`, run `RunTrading`, finalize, and compare against
  `shards=1`. Compare all `Report` scalar fields, `FeesByVenue`, `byCanonical`,
  the sorted `pairs`, the derived stats, `Opps`, and the rest counters.
- Guard assertions so the test can't pass vacuously: `PairsCompleted > 0`,
  `RestsFilled > 0` in maker mode, `PairsFaded > 0`.
- Run it under `go test -race`.

**Verify:** `go test -race ./internal/backtest/...` passes.

### Step 5 — Flip the default, measure, tune

- `simShards()` now defaults to `runtime.NumCPU()`.
- Benchmark on the arch box: time `-maker -days 3` at `PREDBOT_SIM_SHARDS=1`,
  `8`, and the default. Record wall time and peak RSS in this doc. Memory per
  shard is its own `book.Manager` plus in-flight batches; with ~20 shards on
  30 GB this is not expected to matter, but measure it.
- Diff the default run against golden one last time, then run the full archive.
- Follow-ups, **only if the numbers call for them**:
  - Uneven shard CPU in `htop` (a few pinned, most idle) → bin-pack markets onto
    shards by the per-canonical snapshot counts from the id-scan pass.
  - Router or decode now pinned at 100% → raise `stream.maxWorkers` (16) or
    derive it from `NumCPU`.
  - `-adverse` too slow → route trade prints to shards by canonical and give
    each shard its own trade cursor.

### Rollback

`PREDBOT_SIM_SHARDS=1` restores the serial path at runtime without a rebuild.
Steps 1–2 are pure refactors and can stay even if step 3+ is reverted.

## Results (2026-09-22)

What shipped on `backtest-shard-sim`, in commit order:

| Commit | Change |
|---|---|
| `2495290` | Step 1: trading replay moved to `internal/backtest` (`RunTrading`, `shard`) |
| `2b610fb` | Bug fix found along the way: maker pairs were dated at the epoch (`entryTs` 0), so `-maker` reported ~20,800-day holds and a meaningless annualized return. Now dated at fill time, like taker pairs |
| `7036be9` | Step 2: `Report.Merge`, `Finalize` walks pairs in `(entryTs, canonical)` order |
| `ee82769` | Step 3: sharded sim, markets dealt round-robin on first appearance |
| `d0fb4a8` | Step 4: acceptance test (2/5/16 shards == 1 shard, taker and maker) |
| `86e1ee0` | **Not in the original plan:** parallel JSON parse within each file (the real bottleneck, see Problem) |
| `4ec5509` | Step 5: shards default to `GOMAXPROCS` |

Two departures from the plan:

- **Routing is round-robin on first appearance, not a hash.** The router feeds
  shards in time order, so throughput is capped by how well the markets live at
  the same moment are spread. A day's games appear together, so round-robin
  puts them on different shards; a hash can pile several onto one.
- **`-adverse` is a hook in the shard, not a duplicate loop in `main.go`.** It
  runs pinned to one shard because its trade cursor is global.

**Verified on the Pi:**

- Old vs new binary on a synthetic archive (8 markets, 292k snapshots, 5,860
  trade prints): identical output for the default run, `-maker`,
  `-maker -adverse`, the `-export`/`-export-spread` files, and `-hours 1`.
  (After the maker-date fix, only its two annualized/hold lines change.)
- 1 vs 2/3/8/16 shards: byte-identical, including export files.
- `go test -race ./internal/backtest/` passes, including the existing
  early-stop and torn-file stream tests against the new parse pipeline. The
  acceptance test fails as expected when a market's two venues are routed to
  different shards.

**Speed on the Pi (4 cores), 720k-snapshot `-maker` run, 30 markets:**

| Config | Wall | CPU | RSS |
|---|---|---|---|
| serial (`PREDBOT_PARSE_WORKERS=1 PREDBOT_SIM_SHARDS=1`) | 12.7s | 121% | 41 MB |
| parallel parse, 1 shard | 5.1s | 344% | 55 MB |
| parallel parse, 4 shards (default) | 5.4s | 360% | 58 MB |

On 4 cores, parsing alone saturates the box, so sharding adds nothing there.
On the 24-core box the sim becomes the next serial stage, which is what
sharding addresses.

**Still to do, on the arch box with real data:**

1. Golden diff: `PREDBOT_PARSE_WORKERS=1 PREDBOT_SIM_SHARDS=1 ./backtest ... -maker -days 3`
   vs the default. Must be identical.
2. Time the full archive with the defaults and record wall time, CPU% and RSS
   here.
3. If CPU still sits well under 2400%, check `htop` for a single pinned thread:
   that is the consumer (heap merge + router). The next steps would be a
   cheaper merge or raising `maxWorkers`, but only if the numbers call for it.

**Knobs:** `PREDBOT_PARSE_WORKERS` (parse workers per file),
`PREDBOT_SIM_SHARDS` (sim shards), `PREDBOT_REPLAY_WORKERS` (hour look-ahead).
Setting all three to 1 restores the original serial behaviour.
