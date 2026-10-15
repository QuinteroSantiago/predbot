# trading-bots

Two automated trading systems in Go, plus the small library they share.

| Module | What it is | Status |
|---|---|---|
| [`predbot/`](predbot) | Cross-venue arbitrage between Kalshi and Polymarket US prediction markets: order-book recorder, streaming backtester, paper trader, and a risk-gated live trader | Retired 2026-10-14. The edge didn't exist at retail size; see its README for the results |
| [`etfbot/`](etfbot) | Monthly ETF momentum rotation on Alpaca, with a backtester | Built 2026-09 |
| [`tradekit/`](tradekit) | Shared packages: OpenTelemetry setup, hourly gzip JSONL journals, persisted kill switch | Library |

Each directory is its own Go module. The bots import `tradekit` through a
`replace tradekit => ../tradekit` line, and `go.work` ties all three together
for editors.

```
cd predbot && go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs vet, race tests, and a linux/amd64 build
for every module. It never deploys: both bots can place real orders, so
shipping code and arming trading are separate manual steps.

The history of each module was imported from its original repository.

## License

MIT, see [LICENSE](LICENSE).
