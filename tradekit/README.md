# tradekit

Shared Go packages for the trading bots in `~/gomu-apps`. Consumers import it
through a `replace tradekit => ../tradekit` line in their `go.mod`, so it must be
checked out next to them.

| Package | Role |
|---|---|
| `obs` | OpenTelemetry to the homelab LGTM stack; logs teed to stdout for journald |
| `journal` | Hourly-rotated gzip JSONL streams, and keep markers that protect hours from pruning |
| `killswitch` | Kill switch persisted as a marker file; one-shot permission markers |

Only code with a working consumer lives here. Broker, market-data and strategy
interfaces join once a second bot needs them.
