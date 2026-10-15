# etfbot

Monthly ETF rotation on Alpaca, built on [`tradekit`](../tradekit) (a sibling
module in this repo; `go.mod` replaces it).

**Strategy.** On the last trading day of each month, rank the universe (SPY,
QQQ, IWM, EFA, EEM, TLT, IEF, GLD, DBC, VNQ) by the average of 3-, 6- and
12-month total returns and hold the top 3 at a third each. A pick priced
below its 200-day average is replaced by BIL (Treasury bills). Settings:
`config/config.yaml`.

**Backtest** (Yahoo adjusted closes, 5bp per fill):

    go run ./cmd/backtest -from 2007-01-01 -years

**Live run** (`cmd/etfbot`): one run per weekday at 15:45 New York
(`systemd/etfbot.timer`). It trades only on the month's last trading day, and
only with `-live`; a real-money account also needs `-real-money`. State, the
kill switch and the journal live in `-state`:

| File | Meaning |
|---|---|
| `state.json` | holdings the last run left, and the equity high-water mark |
| `KILLED` | kill switch; remove it after review to resume |
| `ADOPT` | one-shot permission to accept holdings the bot didn't leave; consumed by the next live run |
| `journal/` | every plan, fill and refusal, hourly gzip JSONL |

## Layout

| Package | Role |
|---|---|
| `internal/market` | shared types; the `Broker` and `Bars` interfaces |
| `internal/strategy` | `Strategy` interface; `Rotation`, and `Fixed` for benchmarks |
| `internal/rebalance` | targets + holdings → orders; shared by backtest and live |
| `internal/backtest` | monthly replay with slippage, and summary stats |
| `internal/risk` | the gate every plan passes; drawdown kill |
| `internal/state` | what one run leaves for the next |
| `internal/alpaca` | Alpaca trading and data APIs |
| `internal/yahoo` | Yahoo history with a CSV cache, for backtests |
