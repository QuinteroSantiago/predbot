---
title: Prediction-Market Bot — Architecture & Backtester Design (v2)
date: 2026-08-22
status: Retired 2026-10-14 (Phases 1–4 built; see §18)
supersedes: v1 (2026-08-19)
---

# Prediction-Market Bot: Architecture & Backtester Design (v2)

*Go trading bot for cross-venue and intra-venue arbitrage on Kalshi and Polymarket US.
v2 incorporates facts verified against both venues' API docs on 2026-08-22. Where v1 and
v2 disagree, v2 wins. Sections marked **[VERIFY]** must be re-checked against live docs
before the relevant phase ships — both venues change their APIs and fees frequently.*

> **How to use this:** code blocks are skeletons showing shape and intent, not
> copy-paste source. The value is in the decisions. Build in the phase order at the end.

---

## 0. Changes from v1 (read this first)

| Area | v1 said | v2 says | Why |
|---|---|---|---|
| Kalshi feed | snapshot-style; drop frames on backpressure | **delta stream with `seq`; never drop; resync on gap** | Verified: `orderbook_snapshot` then `orderbook_delta`, sequence-numbered |
| Polymarket feed | unknown | **full-snapshot per update; drop-on-backpressure is correct** | Verified: `SUBSCRIPTION_TYPE_MARKET_DATA` sends full top-of-book each time |
| Price type | `Cents int32` | **`Price int64` in $0.0001 units; `Qty int64` in 0.01 contracts** | Both venues use fixed-point decimal strings; Kalshi has sub-penny prices and fractional contracts |
| Book model | YES bids + YES asks | **YES-centric single-instrument view on both venues; derive 4 quotes** | Kalshi returns yes-bids and no-bids only; Polymarket US has one instrument (YES) per market |
| Fees | hardcoded 0.07 Kalshi, "free" Polymarket | **per-venue, per-series, per-role (maker/taker) model** | Kalshi multiplier varies by series; Polymarket US taker 0.06, maker *rebate* 0.0125 |
| Book ownership | Book Manager applies updates | **Ingester owns raw book reconstruction; Book Manager stores consistent snapshots** | Kalshi deltas must be applied where `seq` is checked |
| Risk inputs | filled positions only | **+ in-flight orders, per-venue buying power, unhedged-contract cap** | Two intents can pass the same limit before either fills; capital can't move between venues |
| Kill switch | in-process only | **+ exchange-side backstops** (Kalshi order groups, Polymarket cancel-all) | Process death must not leave resting orders |
| Fills | REST | **private WebSocket on both venues; REST for reconciliation** | Both venues expose fill/order/position streams |
| Phases | 4 | **5 — paper-trading phase inserted before live** | Kalshi demo env is free; `SimExecutor` on live Polymarket feed |
| Jurisdiction | not discussed | **§1 added** | Polymarket.com ≠ Polymarket US; state patchwork |

---

## 1. Jurisdiction & venue identity (non-negotiable)

- **Polymarket US** (`api.polymarket.us`, `gateway.polymarket.us`, docs at `docs.polymarket.us`)
  is a CFTC-regulated US exchange. This is the venue. KYC required. Account creation is via
  the iOS app at time of writing **[VERIFY]**; API keys are issued after KYC.
- **Polymarket.com** (international, Polygon/USDC, `py-clob-client`, "attest you are not a
  U.S. person") is a **different exchange** and is geoblocked for US persons under the 2022
  CFTC settlement. **Do not create, fund, or integrate against it.** Any tutorial mentioning
  CLOB tokens, Polygon wallets, or `py-clob-client` is about the wrong exchange.
- **Kalshi** is CFTC-regulated and available in Florida (operator's state).
- State access is a litigation patchwork that changes monthly, mostly around sports
  contracts. Florida is currently unrestricted on both venues. **Do not VPN around a
  geoblock** — accounts are KYC'd and will be frozen with funds inside.
- **Risk consequence:** a state order can halt a venue or void contracts mid-position
  (Tennessee voided and refunded sports contracts in Jan 2026). This is a legging-risk
  scenario the risk engine must model (§8).

---

## 2. Design goals & non-goals

**Goals**
- One internal representation of "the market," fed by N venues.
- Strategy code that is pure and testable — the same function runs live and in backtest.
- A risk engine that no order can bypass, live or simulated, with exchange-side backstops.
- Observable from line one (OTel → homelab LGTM).
- Deterministic, replayable backtests with a fee/slippage/legging model honest enough
  to tell you the edge isn't there.

**Non-goals**
- Latency competition. No colocation, no microsecond tuning.
- Forecasting. The edge, if any, is market structure.
- Multi-strategy framework generality. Two strategies (cross-venue, intra-venue); generalize only if it pays.

---

## 3. High-level architecture

```
 Kalshi WS  ──▶ [Kalshi Ingester]  ──┐
   (deltas)     owns raw book,       │
                checks seq,          ├─ chan BookSnapshot ─▶ [Book Manager] ─ Snapshot ─▶ [Strategy Loop]
 Poly US WS ──▶ [Poly Ingester]    ──┘                        (stores latest                 decide() pure
   (snapshots)  forwards snapshots                             per venue+market)                  │
                                                                                           chan Intent
 Kalshi private WS ─┐                                                                             ▼
 Poly private WS   ─┴─ chan Fill/OrderUpdate ─▶ [Position & Risk Engine] ◀─── check ─── [Risk Gate]
                                                 (single goroutine owns                          │ approve
                                                  positions, open orders,                        ▼
                                                  buying power per venue)               [Execution Client]
                                                                                                 │
                                                                                          venue REST (place/cancel)
```

**Invariants**
1. **Single-writer state.** Each ingester owns its raw books. The Book Manager owns the
   snapshot map. The Position & Risk Engine owns positions, open orders, and balances.
   Nobody else mutates these; everyone else receives immutable copies over channels.
2. **Strategy → Risk → Execution is the only path to an order.** Strategy proposes; it
   never places. Risk check and position update happen in the same goroutine so there is
   no check-then-act race.
3. **Core imports nothing.** `internal/core` has no I/O and no venue knowledge.

---

## 4. Repo layout

```
predbot/
  cmd/
    record/main.go        # Phase 1: stream both venues, write raw + normalized to disk
    backtest/main.go      # Phase 2: replay → same pipeline → SimExecutor
    paper/main.go         # Phase 3: live feeds → SimExecutor (and/or Kalshi demo env)
    live/main.go          # Phase 4: real execution
  internal/
    core/                 # domain types. NO imports from other internal packages.
      types.go            # Price, Qty, Quote, BookSnapshot, Intent, Order, Fill, Position
      quotes.go           # 4-quote derivation helpers
    venue/
      venue.go            # Venue interface
      kalshi/             # WS (delta+seq), REST V2 orders, private WS, fee lookup
      polymarketus/       # WS (snapshot), REST orders, private WS
    book/                 # Book Manager
    strategy/
      xvenue.go           # cross-venue arb
      intravenue.go       # YES bid + NO bid > 1 on one venue
    risk/                 # Position & Risk Engine (single goroutine)
    exec/                 # Live executor, SimExecutor, legging policy
    backtest/             # ReplaySource, fee models, fade model, report
    obs/                  # OTel setup
    journal/              # append-only order/fill log
  config/
    config.yaml           # venues, market map, risk limits. No secrets.
    markets.yaml          # canonical ID → venue tickers (manually QA'd; see §12)
```

---

## 5. Core data model

```go
package core

// Fixed-point. Both venues send decimal strings; parse exactly, never via float64.
type Price int64 // units of $0.0001. $0.5500 == 5500. Valid range (0, 10000).
type Qty   int64 // units of 0.01 contract. 300 contracts == 30000.

const OneDollar Price = 10000

func ParsePrice(s string) (Price, error) // "0.0550" -> 550; "0.960" -> 9600; reject >4 dp
func ParseQty(s string)   (Qty, error)   // "333.00" -> 33300

type Level struct{ Px Price; Qty Qty }

// Venue-agnostic view of one market on one venue. Everything is expressed on the YES
// instrument. Both venues are single-instrument underneath (see §6.3), so this is exact.
type BookSnapshot struct {
    Venue    string
    VenueID  string      // Kalshi market_ticker / Polymarket marketSlug
    YesBids  []Level     // descending px
    YesAsks  []Level     // ascending px
    TsServer int64       // venue timestamp (Kalshi ts_ms / Poly transactTime), unix nanos, 0 if absent
    TsRecv   int64       // our receive time, unix nanos
    Seq      uint64      // Kalshi seq; 0 for Polymarket
    State    MarketState // open / paused / halted / closed — from venue
}

// Derived quotes. NoBid = 1 - YesAsk, NoAsk = 1 - YesBid.
func (b *BookSnapshot) BestYesBid() (Level, bool)
func (b *BookSnapshot) BestYesAsk() (Level, bool)
func (b *BookSnapshot) BestNoBid()  (Level, bool)   // = OneDollar - BestYesAsk
func (b *BestNoAsk …)

type Side uint8 // BuyYes, SellYes. "Buy NO" is expressed as SellYes everywhere internally.

type Intent struct {
    PairID   string // groups the two legs of an arb; "" for single-leg
    Leg      int    // 0 or 1
    Venue    string
    VenueID  string
    Side     Side
    Px       Price  // limit price on the YES instrument
    Qty      Qty
    TIF      TIF    // IOC for taking legs; GTC/GTD for resting legs
    Reason   string
}

type Order struct {
    Intent
    ClientID string // idempotency key. Kalshi: client_order_id. Polymarket: [VERIFY field name]
}

type Fill struct {
    OrderClientID string
    Venue         string
    Px            Price
    Qty           Qty
    Fee           int64 // signed, $0.0001 units; negative == rebate
    IsMaker       bool
    TsFilled      int64
}
```

**Decisions to defend**
- Fixed-point `int64`, not floats and not `int32` cents: Kalshi quotes to $0.0001 and 0.01
  contracts; Polymarket quantities may carry decimals.
- Every order has a client-generated idempotency key; retries reconcile rather than resubmit.
- "Buy NO" does not exist as a first-class side. It's `SellYes`. This removes an entire
  class of sign bugs and matches how both venues actually work.

---

## 6. Venue facts (verified 2026-08-22)

### 6.1 Kalshi

| Item | Fact |
|---|---|
| Market data WS | `wss://external-api-ws.kalshi.com` (prod); **auth required even for public channels** |
| Orderbook channel | `orderbook_snapshot` first, then `orderbook_delta`. Each carries `sid` and `seq`. Subscribe with `market_tickers[]`. `update_subscription` supports `add_markets`, `delete_markets`, `get_snapshot` |
| Snapshot shape | `yes_dollars_fp: [[price_dollars, qty_fp], …]`, `no_dollars_fp: […]`. Keys absent if that side is empty. **These are bids only, on each side.** |
| Delta shape | `price_dollars`, `delta_fp` (signed), `side: yes\|no`, optional `ts_ms`, optional `client_order_id` (present when *your* order caused the change — free fill/ack signal) |
| Prices | fixed-point dollar strings, up to 4 dp (sub-penny); fractional contracts to 2 dp |
| Orders | Create Order **V2** (`bid`/`ask` on single book, fixed-point dollars) with `client_order_id`. Legacy `/portfolio/orders` deprecating. Batch create/cancel sized by rate tier. Amend, Decrease endpoints |
| Order groups | Rolling 15-second contract limit; breach → all orders in group cancelled and new orders blocked until reset. **Use as exchange-side kill backstop.** |
| Private WS | `user_fills`, `user_orders`, `market_positions`, `order_group_updates` |
| Fees | Taker ≈ `mult × C × P × (1−P)`, base mult 0.07, **varies by series/event** — read `GET /series/fee_changes` and `GET /events/fee_changes` at startup. Maker fee exists (reportedly ~25% of taker) **[VERIFY in official PDF]**. Trade fee ceiled to $0.0001; then a rounding fee to the account's balance precision ($0.0001 direct member / $0.01 otherwise); per-order accumulator issues $0.01 rebates |
| Rate limits | Token-bucket tiers (Basic → Prestige). `GET /account/api_limits`. **[VERIFY tier before sizing subscriptions]** |
| Demo env | Exists. Use for Phase 3 |
| Historical | Trades and candlesticks via API; **no historical L2 book** → recording is required |
| Sharding | `exchange_index` appears on balance/transfer endpoints. Default 0. **[VERIFY whether it affects market data]** |
| Interest | Kalshi reportedly pays interest on idle balance **[VERIFY rate]** — this is the opportunity cost in §10.4 |

### 6.2 Polymarket US

| Item | Fact |
|---|---|
| Hosts | Trading `https://api.polymarket.us` (auth). Public data `https://gateway.polymarket.us` (no auth) |
| Market data WS | `wss://api.polymarket.us/v1/ws/markets`, **auth required**. Types: `MARKET_DATA` (full book + stats), `MARKET_DATA_LITE` (BBO only), `TRADE` |
| Book shape | Full snapshot each message: `bids[]`, `offers[]` of `{px:{value,currency}, qty}`, sorted best-first, **top levels only**, `state`, `transactTime`. Optional `responsesDebounced: true` batches updates |
| Subscription limit | 100 markets per subscription; open multiple |
| Instrument model | **One instrument per market (YES).** Buy NO == sell YES. Price always refers to YES |
| Orders | Limit / Market. TIF: GTC, GTD, IOC, FOK. Endpoints: create, modify, cancel, cancel-all (filter by slug), preview, close-position, batch ×20. **Batch outcomes arrive on the order stream, not the HTTP response** |
| Private WS | `wss://api.polymarket.us/v1/ws/private` — orders, positions, balances |
| Collateral | Fully collateralized. Long pays price; **short posts $1.00 margin per contract**, buying power falls by (1 − price). Trade fails if buying power < 0. No margin calls. Portfolio margin for mutually-exclusive outcomes |
| Fees (from 2026-07-01) | `Fee = Θ × C × p × (1−p)`. **Taker Θ = 0.06. Maker Θ = −0.0125 (a rebate).** Banker's rounding to $0.01. Taker volume rebate tiers from $250k/month |
| Contracts | Docs conflict: "whole contracts only" (retail) vs "qty may contain decimals for partial-contract markets" **[VERIFY for your account]** |
| Trading hours | A trading-hours / maintenance page exists **[VERIFY — not 24/7 may create venue-close legging risk]** |
| Rate limits | Page exists **[VERIFY]** |
| Access | iOS app for signup/KYC; API keys after KYC **[VERIFY current state]** |

### 6.3 Normalization — both venues are YES-centric single-instrument

```
Kalshi:       YesBids = yes_dollars_fp (as-is)
              YesAsks = for each no-bid (p, q): (1 − p, q), sorted ascending
Polymarket:   YesBids = bids, YesAsks = offers (as-is)
```

After this, the strategy never thinks about NO books. "Buy NO at 0.40 on X" is
"SellYes at 0.60 on X," which hits X's YesBid.

---

## 7. Ingesters (Phase 1)

### 7.1 Kalshi — delta stream, never drop

```go
func (k *Kalshi) Stream(ctx context.Context, out chan<- core.BookSnapshot) error {
    backoff := 250 * time.Millisecond
    for ctx.Err() == nil {
        conn, err := k.dial(ctx)
        if err != nil { sleep(ctx, backoff); backoff = min(backoff*2, 30*time.Second); continue }
        backoff = 250 * time.Millisecond
        k.books = map[string]*rawBook{}          // discard everything on reconnect
        err = k.readLoop(ctx, conn, out)
        metrics.Disconnects.Add(ctx, 1, venueAttr("kalshi"))
        _ = conn.Close()
    }
    return ctx.Err()
}

func (k *Kalshi) readLoop(ctx context.Context, c *ws.Conn, out chan<- core.BookSnapshot) error {
    var expectSeq uint64
    for {
        raw, err := c.Read(ctx)
        if err != nil { return err }
        k.recorder.WriteRaw("kalshi", raw, time.Now())   // Phase 1: raw frames always recorded
        msg := parse(raw)
        switch msg.Type {
        case "orderbook_snapshot":
            k.books[msg.Ticker] = fromSnapshot(msg); expectSeq = msg.Seq + 1
        case "orderbook_delta":
            if msg.Seq != expectSeq {                      // GAP: do not apply, do not drop
                metrics.SeqGaps.Add(ctx, 1)
                k.requestSnapshot(ctx, c, msg.Ticker)      // update_subscription get_snapshot
                continue
            }
            expectSeq++
            k.books[msg.Ticker].apply(msg)                 // delta_fp may be negative; remove level at 0
        default:
            continue
        }
        snap := k.books[msg.Ticker].normalize(time.Now()) // §6.3; immutable copy
        select {
        case out <- snap:
        case <-ctx.Done(): return ctx.Err()
        default:
            // Consumer behind. The raw book here is still correct because WE applied the
            // delta. It's safe to skip emitting this snapshot — the next emit carries the
            // latest state. This is conflation, not data loss. Count it.
            metrics.ConflatedSnapshots.Add(ctx, 1)
        }
    }
}
```

> **Interview point:** a delta feed cannot be dropped, but the *derived snapshot* can be
> conflated because the ingester owns the authoritative book. Gap handling is resync, not
> reconnect. Reconnect discards all books because `seq` restarts.

### 7.2 Polymarket US — snapshot stream, drop-on-backpressure

```go
func (p *PolyUS) readLoop(ctx context.Context, c *ws.Conn, out chan<- core.BookSnapshot) error {
    for {
        raw, err := c.Read(ctx)
        if err != nil { return err }
        p.recorder.WriteRaw("polymarketus", raw, time.Now())
        md, ok := parseMarketData(raw); if !ok { continue }
        snap := normalizePoly(md, time.Now())
        select {
        case out <- snap:
        case <-ctx.Done(): return ctx.Err()
        default:
            metrics.DroppedSnapshots.Add(ctx, 1) // stale full snapshot; next one supersedes it
        }
    }
}
```

Use `responsesDebounced: true` initially; turn it off if the fade model (§10.3) shows
you need every update. Subscribe in batches of ≤100 slugs.

### 7.3 Recording format (Phase 1 deliverable)

- **Raw**: one JSONL file per venue per hour, gzip, line = `{ts_recv_ns, venue, frame}`.
  Raw frames are the ground truth; everything else can be regenerated.
- **Normalized**: one JSONL per hour, line = `core.BookSnapshot`. Convenience for backtest.
- Rotate hourly, fsync on rotate, `SIGHUP` closes files cleanly.
- Estimate: a few dozen markets, debounced Poly + Kalshi deltas ≈ low hundreds of MB/day
  compressed. Move to length-prefixed protobuf only if this becomes a problem.
- Record the `TsRecv − TsServer` distribution as a metric from day one; it is both your
  clock-sync check and your feed-latency monitor. Run `chrony` on the host.

---

## 8. Position & Risk Engine (Phase 2)

One goroutine owns positions, open orders, and per-venue buying power, and performs the
risk check. This eliminates the check-then-act race between "approve intent" and "fill
arrives."

```go
type State struct {
    pos       map[key]Qty            // filled, net YES contracts (negative == short)
    inflight  map[key]Qty            // approved-but-unfilled, signed
    bp        map[venue]int64        // buying power from private WS, $0.0001 units
    unhedged  map[pairID]Qty         // one leg filled, other not
    killed    bool
}

func (s *State) Check(i core.Intent) error {
    switch {
    case s.killed:                                         return ErrKilled
    case i.Qty > s.cfg.MaxOrderQty:                        return ErrOrderTooBig
    case i.Px <= 0 || i.Px >= core.OneDollar:              return ErrSanity
    case abs(s.pos[k]+s.inflight[k]+signed(i)) > s.cfg.MaxPos[k]: return ErrPositionCap
    case s.bp[i.Venue]-bpCost(i) < s.cfg.MinBP[i.Venue]:   return ErrBuyingPower
    case totalUnhedged(s) > s.cfg.MaxUnhedged:             return ErrUnhedgedCap
    case s.bookAge[i.Venue] > s.cfg.MaxBookAge:            return ErrStaleBook
    case s.marketState[k] != Open:                         return ErrMarketNotOpen
    }
    return nil
}

// Buying-power cost must match each venue's collateral model:
//   BuyYes at p:  p × qty                       (both venues)
//   SellYes at p: (1 − p) × qty                 (Polymarket US posts $1 margin, receives p)
//                 Kalshi: [VERIFY] same economics; confirm field semantics in balance API
```

**Kill wiring (`Kill(reason)`):** P&L floor breach; book staleness; seq-gap storm; N venue
errors in M seconds; unhedged cap breach for longer than T; manual (`SIGUSR1`, HTTP).
`Kill()` does, in order: set `killed`; persist a `KILLED` marker file so a restart stays
killed; Polymarket `cancel-all`; Kalshi `trigger order group`; log + metric.

**Exchange-side backstops (configured at startup, independent of the process):**
- Kalshi: every order is placed inside one order group with a rolling 15-second contract limit
  sized to `MaxUnhedged`. If the bot goes insane, the exchange cuts it off.
- Polymarket US: no equivalent; rely on GTD expiry on resting orders (never GTC) so a dead
  process leaves nothing resting for long.

**Startup reconciliation (before any intent is accepted):** pull open orders, positions,
balances from both venues via REST; cancel anything unknown; load positions; only then
subscribe the strategy.

**Legging policy (product decision: "sell it"):**
1. Leg order: place the leg on the venue with the *thinner* top-of-book first (higher
   fade risk); on fill, immediately place leg 2 as IOC.
2. If leg 2 is unfilled or partial after `LegTimeout` (config, start 2s): place an IOC
   exit of the unhedged quantity on leg 1's venue at the best available price.
3. Book the round-trip cost as `legging_cost` on the pair. It is a first-class P&L line.
4. If the exit also fails (venue halt, no bid): escalate — `unhedged` stays elevated,
   `Kill()` fires at the cap, a human decides. Do not auto-retry into a halted market.

---

## 9. Strategies (Phase 2, detect-only first)

Both are `func Decide(Snapshot, Exposure) []Intent`. `Exposure` carries positions, in-flight
orders, and unhedged pairs so the strategy does not re-emit an opportunity it is already
working. No I/O.

### 9.1 Cross-venue

For each canonical market with venues A, B:
```
edge_AB = BestYesBid(B).px − BestYesAsk(A).px − fee(A, taker, askA) − fee(B, taker, bidB)
edge_BA = symmetric
```
If `edge > minEdge` (config, must be > 0 after fees; start at 100 = 1¢): emit a pair
(BuyYes on A at askA, SellYes on B at bidB), qty = min(depth at those levels, maxClip).
Also compute `annualized` (§10.4) and require `annualized > minAnnualized`.

### 9.2 Intra-venue

On one venue: `BestYesBid + BestNoBid > OneDollar + fees`. Equivalently `YesBid > YesAsk`,
which is a crossed book and essentially never persists on a matched CLOB — but it can appear
transiently in Kalshi's two-sided representation during delta application, and it is
**resolution-risk-free**. Detect-only; it is mostly a feed-consistency check.

**Maker variants (Phase 4+, only if taker edge isn't there):** rest one leg as a GTD
limit, take the other on fill. On Polymarket US the resting leg *earns* Θ=0.0125. The
backtester must model fill probability for resting orders (queue position is not
observable on Polymarket; it is on Kalshi via the queue-position endpoints).

### 9.3 Cost reality check (for calibration)

At p = 0.50, taker fees are ≈ 1.5¢/contract (Poly US) + ≈ 1.75¢/contract (Kalshi) → a
two-leg taker arb needs > ~3.25¢ of gross cross-venue gap before slippage and fade. At
p = 0.10 or 0.90 the combined fee is ≈ 1.2¢. Expect the backtester to show that mid-price
taker arb is rare; tails and maker-legged variants are where to look. **Finding "no edge"
with receipts is a successful Phase 2.**

---

## 10. Backtester (Phase 2)

Replays recorded `BookSnapshot`s through the same Book Manager, Strategy, and Risk Engine
with `SimExecutor` in place of the venues. Deterministic: no wall-clock, seeded RNG.

### 10.1 Replay

Merge both venues' normalized streams ordered by `TsRecv`. Expose a virtual clock to the
pipeline (inject `Clock` interface; never call `time.Now()` outside `obs/` and ingesters).

### 10.2 Fill model

- **Walk the book.** Average fill across levels; partial if depth is exhausted.
- **Latency Δ** (config, start 150 ms): an order placed at `t` is matched against the
  snapshot at `t + Δ`, not `t`. Because the recorded stream has both venues, fade is
  *measured*, not modeled: if the level is gone at `t + Δ`, you didn't get it.
- **Top-levels-only caveat:** Polymarket's feed carries only top levels; sizes beyond
  that are unknown → treat as unavailable (conservative).

### 10.3 Fee model

```go
type FeeModel interface {
    Fee(venue string, series string, px core.Price, qty core.Qty, maker bool) int64 // signed
}
```
- Kalshi: `ceil4(mult[series] × C × P × (1−P))` with series multipliers loaded from a
  config snapshot taken from the fee-changes API; then rounding fee + accumulator per
  order (§6.1). Implement the accumulator exactly — it's small and it's a correctness story.
- Polymarket US: `bankersRound2(Θ × C × p × (1−p))`, Θ = 0.06 taker / −0.0125 maker.
- Property tests: fee ≥ 0 for takers; |fee| monotone in qty; symmetric in p ↔ 1−p;
  Kalshi accumulator converges to single-fill fee.

### 10.4 Capital & lock-up

Every pair locks collateral until resolution. Report per opportunity and in aggregate:
```
capital_locked = bpCost(leg1) + bpCost(leg2)
days           = resolution_date − t
annualized     = net_edge / capital_locked × 365 / days
```
Compare against the idle-balance yield on Kalshi **[VERIFY rate]** as the hurdle. With
$10k per venue, a 3¢ net edge locking ~$1 for 30 days is ~37% annualized; the same edge
locking for 300 days is ~3.7% and likely loses to idle yield.

### 10.5 Report (the only output that matters)

```
Gross gap captured
Fees (per venue, maker/taker split)
Slippage (fill − top-of-book)
Fade losses (opportunities that vanished within Δ)
Legging cost (round-trip exits on half-filled pairs)       ← first-class line
Net P&L
Capital-weighted annualized return
Sharpe / hit rate / max drawdown
Pairs attempted / completed / legged / abandoned
Resolution-mismatch exposure (pairs whose venues resolve on different sources)
```

Golden-file test: fixed recorded day + expected report; any refactor that changes a
number fails CI.

---

## 11. Execution Client (Phase 4)

- Kalshi: Create Order V2 with `client_order_id`; order group membership mandatory.
  Fills arrive on `user_fills` WS; the `client_order_id` echoed on `orderbook_delta` is a
  faster ack.
- Polymarket US: create with TIF (IOC for taking legs, GTD for resting); **read outcomes
  from the private WS**, not the HTTP response, especially for batch. Use `preview` in
  paper mode to validate price/qty semantics against the venue before risking money.
- Retry policy: on timeout/5xx, **do not resubmit**; query by client ID (Kalshi) or open
  orders (Polymarket) and reconcile. A retried submit without reconcile is how positions
  double.
- 500 ms per-call timeout; legs are sequenced per §8.

---

## 12. Market mapping

`config/markets.yaml`:
```yaml
- canonical: FED-2026-09-CUT-25
  resolves_note: "Kalshi: FOMC statement. Poly US: [source]. Same? YES/NO — QA'd by <name> on <date>"
  resolution_date: 2026-09-17
  venues:
    kalshi: { ticker: "FED-26SEP-T…" }
    polymarketus: { slug: "fed-…" }
```
Manual QA for now. Every entry must state both venues' resolution sources and whether
they are identical; a mismatch is not tradeable regardless of edge. Later: a classifier
seeded from this hand-labeled set. The Book Manager refuses to key a snapshot to a
canonical ID that isn't in this file.

---

## 13. Observability

OTLP to the homelab LGTM stack from Phase 1. Minimum metric set:

```
predbot_book_age_seconds{venue}                 histogram
predbot_feed_latency_seconds{venue}             histogram (ts_recv − ts_server)
predbot_seq_gaps_total{venue}                   counter
predbot_conflated_snapshots_total{venue}        counter
predbot_dropped_snapshots_total{venue}          counter
predbot_reconnects_total{venue}                 counter
predbot_tick_to_decision_seconds                histogram
predbot_decision_to_ack_seconds{venue}          histogram
predbot_intents_total{strategy,result}          counter (result: approved/rejected:<reason>)
predbot_orders_total{venue,result}              counter
predbot_unhedged_contracts                      gauge
predbot_buying_power{venue}                     gauge
predbot_pnl{kind=gross|fees|slippage|legging|net}  gauge
predbot_killed                                  gauge (0/1)
```
Structured logs with `pair_id` on every line touching an order. Append-only journal of
every intent, order, ack, fill, and kill to disk so P&L can be recomputed from it.

---

## 14. Config & secrets

- `config.yaml`: venues, risk limits, `minEdge`, `minAnnualized`, `maxClip`, `LegTimeout`,
  latency Δ for backtest, Kalshi fee multiplier snapshot. No secrets.
- Secrets: `/etc/predbot/env`, root:root 0600, loaded by hardened systemd unit
  (dedicated user, `ReadWritePaths` scoped to data + journal dirs). Kalshi private RSA key
  and Polymarket US API key never logged.
- `Restart=on-failure`; the `KILLED` marker file (§8) survives restarts and must be
  removed by a human.

---

## 15. Testing

- `go test -race ./...` in Gitea Actions on every push.
- Ingester chaos tests: truncated frames, out-of-order `seq`, reconnect mid-delta,
  reconnect mid-subscription; assert no panic, assert resync, assert book equals a
  reference rebuilt from raw.
- Property tests on fee models (§10.3) and fill model (avg fill ≥ best ask when buying;
  monotone in qty).
- Golden-file backtest.
- Legging-policy state machine tests: every (leg1 state × leg2 state × venue state)
  combination has a defined action; halted-venue path never retries.

---

## 16. Open items — resolve before the marked phase

| # | Item | Blocks |
|---|---|---|
| 1 | Kalshi: official maker fee, per-series multipliers, idle-balance interest rate | Phase 2 fee model |
| 2 | Kalshi: API rate tier for this account; max subscribed markets; `exchange_index` effect on market data | Phase 1 |
| 3 | Polymarket US: rate limits page; trading hours / maintenance window | Phase 1 / §8 |
| 4 | Polymarket US: whole vs fractional contracts for this account; idempotency field on create-order | Phase 4 |
| 5 | Polymarket US: API key issuance flow after iOS KYC | Phase 1 (can record via public `gateway` REST polling if WS auth is delayed) |
| 6 | Kalshi SellYes buying-power semantics vs Polymarket's $1-margin model | Phase 2 risk |
| 7 | Does Polymarket US publish resolution source per market in the API? | §12 |

---

## 17. Build order

1. **Phase 1 — Record (Q1).** `core` types, both ingesters with raw + normalized recording,
   metrics, `cmd/record` under systemd. No trading, no strategy. Deliverable: weeks of
   data and a dashboard showing feed latency, seq gaps, and book age per venue.
2. **Phase 2 — Backtest (Q2).** Book Manager, both strategies detect-only, Position &
   Risk Engine, backtester with fill/fee/fade/lock-up models and the §10.5 report.
   Deliverable: a number, positive or negative, with receipts.
3. **Phase 3 — Paper (Q2–Q3).** `cmd/paper`: live feeds → full pipeline → `SimExecutor`;
   in parallel, real orders on the Kalshi demo environment. Deliverable: live-vs-backtest
   P&L divergence report (this is where hidden bugs show up).
4. **Phase 4 — Live (Q3).** Execution client, legging policy, exchange-side backstops,
   startup reconciliation. Tuition-sized limits well under the $10k/venue. Operate it.
5. **Phase 5 — Freeze & present (Q4).** Polish this document with real numbers, the
   Grafana dashboard, and the backtest-vs-live divergence analysis. Stop adding features.

Each phase is independently demo-able. Stopping after Phase 2 with "here is why there is
no retail-speed edge, measured" is a complete project.

---

## 18. Outcome (added 2026-10-14)

*Added when the project was retired. Sections 0–17 above are the design as written
on 2026-08-22 and are left unedited. Where the build departed from them, the
README and the code are authoritative.*

**Phases 1–4 were built and run with real money; Phase 5 became this section.**
§9.3 and §17 predicted the result: no retail-speed edge, with measurements to
show it.

### What was measured

| Test | Result |
|---|---|
| Live taker pairs | 58 cross-venue pairs logged; **1** hedged at its quoted prices. The Polymarket hedge quote was usually gone ~1 s after the Kalshi leg filled, so fade (§10.2) dominated |
| Taker backtest, full archive (~83 GB) | **+$83** total, about half of it from 3 games. Taker/taker almost never clears the ~3.49¢ round-trip fee wall at retail depth (§9.3 estimated ~3.25¢ at p = 0.50) |
| Maker-rebate variant (§9.2) | Rest the Polymarket leg for the Θ = 0.0125 rebate, hedge on Kalshi. Backtest adverse drift (0.287¢) sat just under the rebate (0.31¢), a margin too thin to survive live conditions |
| Lead-lag, longshot-bias, statistical models | No edge on the recorded data |
| Capital | Wound down flat; no capital lost |

### Where the build departed from the design

- **Universe.** §12's hand-QA'd `markets.yaml` basket (elections, macro, crypto) was
  dropped on 2026-08-28 because its edges were phantom. The bot moved to
  cross-listed MLB and NFL games discovered at runtime on both venues.
- **Trade prints.** The recorder added a trade stream per venue. A book alone can't
  tell a trade from a cancel, so adverse selection can't be measured without it.
- **Retention.** §7.3's "low hundreds of MB/day" underestimated the volume. Edge
  keep-markers pinned ~95% of hours at the 2¢ threshold, so VM pruning stopped
  honoring them and a separate archive host kept the full history instead.
- **Backtest scale.** The streaming k-way replay (§10.1) kept memory bounded, but a
  full-archive run took over 3 h on a Raspberry Pi 5. Full runs moved to a 24-core
  x86 box and were sharded across cores.

### Lessons

1. **Measure fade before building execution.** Recording both venues and replaying
   at `t + Δ` (§10.2) gave the right answer in Phase 2; the live run only confirmed it.
2. **Fees set the bar.** At retail size, a cross-venue gap is real but rarely larger
   than both venues' taker fees combined.
3. **Maker rebates are thin.** A rebate only pays if adverse selection stays below it,
   and the measured margin was a few hundredths of a cent.
4. **Safety rails are what make a live test cheap.** The risk gate, persisted kill
   switch, and preflight reconcile let the live run stay tuition-sized and be
   stopped at any time.
