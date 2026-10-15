// etfbot runs the monthly ETF rotation against an Alpaca account. It is meant
// to run once each weekday near the close (systemd/etfbot.timer); it trades only
// on the last trading day of the month.
//
// It is a dry run unless -live is given, and -live refuses a real-money account
// unless -real-money is given too. Every run:
//
//  1. stops if the kill switch (<state>/KILLED) is engaged;
//  2. stops unless the market is open and it's the month's last trading day;
//  3. checks the account against the holdings the last run left, and refuses
//     on any difference unless a human left a one-shot <state>/ADOPT marker;
//  4. engages the kill switch if equity is MaxDrawdown below its peak;
//  5. computes targets, plans orders, and passes the whole plan through the
//     risk gate before sending any of it;
//  6. sells, waits for the fills, then buys with the cash actually raised.
//
// Credentials come from APCA_API_KEY_ID and APCA_API_SECRET_KEY; the account
// from APCA_API_BASE_URL (default: paper).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"tradekit/journal"
	"tradekit/killswitch"
	"tradekit/obs"

	"etfbot/internal/alpaca"
	"etfbot/internal/config"
	"etfbot/internal/market"
	"etfbot/internal/rebalance"
	"etfbot/internal/risk"
	"etfbot/internal/state"
)

// build names the commit this binary was built from; set at link time with
// -ldflags "-X main.build=<sha>+tradekit.<sha>".
var build = "dev"

// staleAfter is how old a latest trade may be before the price is refused.
const staleAfter = 15 * time.Minute

type flags struct {
	cfg, dir        string
	live, realMoney bool
	force           bool
}

func main() {
	var f flags
	flag.StringVar(&f.cfg, "config", "config/config.yaml", "configuration file")
	flag.StringVar(&f.dir, "state", "data", "state directory: state.json, KILLED, ADOPT, journal/")
	flag.BoolVar(&f.live, "live", false, "place orders (default: dry run, log the plan only)")
	flag.BoolVar(&f.realMoney, "real-money", false, "with -live, allow a real-money (non-paper) account")
	flag.BoolVar(&f.force, "force", false, "run even if today isn't the month's last trading day")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown := obs.Setup(ctx, "etfbot")
	defer shutdown(context.Background())

	if err := run(ctx, f); err != nil {
		slog.Error("run failed", "err", err)
		shutdown(context.Background())
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags) error {
	cfg, err := config.Load(f.cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		return err
	}
	kill := killswitch.New(filepath.Join(f.dir, "KILLED"))
	if kill.Killed() {
		return errors.New(kill.Reason())
	}
	jr, err := journal.NewStream(f.dir, "journal")
	if err != nil {
		return err
	}
	defer jr.Close()
	record := func(event string, fields map[string]any) {
		fields["event"], fields["build"], fields["ts"] = event, build, time.Now().UTC()
		if b, err := json.Marshal(fields); err == nil {
			_ = jr.Write("etfbot", b, time.Now())
		}
	}

	ac := &alpaca.Client{BaseURL: alpaca.PaperURL, DataURL: alpaca.DataURL,
		KeyID: os.Getenv("APCA_API_KEY_ID"), Secret: os.Getenv("APCA_API_SECRET_KEY")}
	if u := os.Getenv("APCA_API_BASE_URL"); u != "" {
		ac.BaseURL = u
	}
	if ac.KeyID == "" || ac.Secret == "" {
		return errors.New("APCA_API_KEY_ID and APCA_API_SECRET_KEY must be set")
	}
	if f.live && !ac.Paper() && !f.realMoney {
		return fmt.Errorf("%s is a real-money account: pass -real-money as well as -live", ac.BaseURL)
	}
	mode := map[bool]string{false: "dry run", true: "LIVE"}[f.live]
	slog.Info("etfbot run", "build", build, "mode", mode, "account", ac.BaseURL)

	// 2. Only near the close of the month's last trading day.
	open, now, err := ac.Clock(ctx)
	if err != nil {
		return err
	}
	today := market.DayOf(now)
	if !open && f.live {
		slog.Info("market closed; nothing to do")
		return nil
	}
	days, err := ac.TradingDays(ctx, today, today.AddDate(0, 0, 14))
	if err != nil {
		return err
	}
	if !lastOfMonth(today, days) && !f.force {
		slog.Info("not the month's last trading day; nothing to do", "today", today.Format(time.DateOnly))
		return nil
	}

	// 3. The account must be as the last run left it.
	acct, err := ac.Account(ctx)
	if err != nil {
		return err
	}
	held, err := holdings(ctx, ac)
	if err != nil {
		return err
	}
	statePath := filepath.Join(f.dir, "state.json")
	st, found, err := state.Load(statePath)
	if err != nil {
		return err
	}
	diff := state.Diff(st.Holdings, held)
	if !found && len(held) == 0 {
		diff = nil
	}
	if len(diff) > 0 {
		adopt := filepath.Join(f.dir, "ADOPT")
		ok := false
		if f.live {
			if ok, err = killswitch.ConsumeMarker(adopt); err != nil {
				return fmt.Errorf("adopt marker could not be consumed, refusing: %w", err)
			}
		} else if _, err := os.Stat(adopt); err == nil {
			ok = true // a dry run reads the marker but leaves it for the live run
		}
		if !ok {
			record("refused", map[string]any{"reason": "holdings changed", "diff": diff})
			return fmt.Errorf("account holdings differ from the last run's %v; review them, then touch %s to adopt them for one run", diff, adopt)
		}
		slog.Warn("adopting holdings the bot didn't leave", "diff", diff)
	}

	// 4. Drawdown stop.
	peak := math.Max(st.PeakEquity, acct.Equity)
	g := risk.New(cfg, kill)
	if g.Drawdown(acct.Equity, peak) {
		record("killed", map[string]any{"reason": kill.Reason()})
		return errors.New(kill.Reason())
	}

	// 5. Targets, plan, risk gate.
	strat := cfg.Rotation()
	syms := strat.Symbols()
	hist, err := ac.Daily(ctx, syms, today.AddDate(-2, 0, 0), today.AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	quoteSyms := append([]string{}, syms...)
	for sym := range held {
		quoteSyms = append(quoteSyms, sym)
	}
	quotes, err := ac.Latest(ctx, quoteSyms)
	if err != nil {
		return err
	}
	prices := map[string]float64{}
	for _, sym := range quoteSyms {
		q, ok := quotes[sym]
		if !ok || q.Price <= 0 {
			return fmt.Errorf("no latest price for %s", sym)
		}
		if open && now.Sub(q.At) > staleAfter {
			return fmt.Errorf("%s's latest trade is %s old; refusing a stale price", sym, now.Sub(q.At).Round(time.Second))
		}
		prices[sym] = q.Price
	}
	for _, sym := range syms {
		b := hist[sym]
		if len(b) == 0 || today.Sub(b[len(b)-1].Date) > 7*24*time.Hour {
			return fmt.Errorf("history for %s is missing or stale", sym)
		}
		if open { // today's bar, at the current price
			hist[sym] = append(b, market.Bar{Date: today, Close: prices[sym]})
		}
	}
	targets, err := strat.Targets(today, hist)
	if err != nil {
		return err
	}
	orders := rebalance.Plan(held, acct.Cash, prices, targets, cfg.Params())
	if err := g.Check(targets, orders); err != nil {
		record("refused", map[string]any{"reason": err.Error(), "targets": targets, "orders": orders})
		return err
	}
	for _, o := range orders {
		if err := g.CheckSellSize(o, prices[o.Symbol]); err != nil {
			record("refused", map[string]any{"reason": err.Error(), "orders": orders})
			return err
		}
	}
	slog.Info("plan", "equity", acct.Equity, "targets", fmtWeights(targets), "orders", len(orders))
	for _, o := range orders {
		slog.Info("order", "side", o.Side.String(), "symbol", o.Symbol, "qty", o.Qty, "usd", o.Notional)
	}
	record("plan", map[string]any{"mode": mode, "equity": acct.Equity, "targets": targets, "orders": orders, "prices": prices})
	if !f.live {
		slog.Info("dry run: no orders placed")
		return nil
	}

	// 6. Sells, then buys with the cash actually raised. Any execution failure
	// engages the kill switch: a half-done rebalance needs a human.
	fail := func(err error) error {
		kill.Kill("execution: " + err.Error())
		if h, herr := holdings(ctx, ac); herr == nil {
			st.Holdings = h
			_ = state.Save(statePath, st)
		}
		record("killed", map[string]any{"reason": kill.Reason()})
		return err
	}
	var buys []market.Order
	for _, o := range orders {
		if o.Side == market.Buy {
			buys = append(buys, o)
			continue
		}
		if err := execute(ctx, ac, o, record); err != nil {
			return fail(err)
		}
	}
	if len(buys) > 0 {
		acct, err = ac.Account(ctx)
		if err != nil {
			return fail(err)
		}
		for _, o := range rebalance.FitBuys(buys, acct.Cash*(1-cfg.Rebalance.CashBuffer)) {
			if err := g.Check(nil, []market.Order{o}); err != nil {
				return fail(err)
			}
			if err := execute(ctx, ac, o, record); err != nil {
				return fail(err)
			}
		}
	}

	if st.Holdings, err = holdings(ctx, ac); err != nil {
		return fail(err)
	}
	if acct, err = ac.Account(ctx); err != nil {
		return fail(err)
	}
	st.PeakEquity, st.LastRun = math.Max(peak, acct.Equity), time.Now().UTC()
	if err := state.Save(statePath, st); err != nil {
		return fail(err)
	}
	record("done", map[string]any{"equity": acct.Equity, "holdings": st.Holdings})
	slog.Info("rebalanced", "equity", acct.Equity, "holdings", st.Holdings)
	return nil
}

// execute places o and waits for it to finish; anything short of a full fill
// is an error.
func execute(ctx context.Context, b market.Broker, o market.Order, record func(string, map[string]any)) error {
	id, err := b.Place(ctx, o)
	if err != nil {
		return fmt.Errorf("place %s %s: %w", o.Side, o.Symbol, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, err := b.Order(ctx, id)
		if err != nil {
			return fmt.Errorf("order %s: %w", id, err)
		}
		if st.Done {
			record("fill", map[string]any{"order": o, "id": id, "status": st.Status, "filled_qty": st.FilledQty, "avg_price": st.AvgPrice})
			if st.Status != "filled" {
				return fmt.Errorf("%s %s ended %s", o.Side, o.Symbol, st.Status)
			}
			slog.Info("filled", "side", o.Side.String(), "symbol", o.Symbol, "qty", st.FilledQty, "price", st.AvgPrice)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s not filled after 2m (status %s)", o.Side, o.Symbol, st.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func holdings(ctx context.Context, b market.Broker) (map[string]float64, error) {
	ps, err := b.Positions(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, p := range ps {
		out[p.Symbol] = p.Qty
	}
	return out, nil
}

// lastOfMonth reports whether today is a trading day and the next trading day
// falls in another month. days are the trading days from today onward.
func lastOfMonth(today time.Time, days []time.Time) bool {
	if len(days) < 2 || !days[0].Equal(today) {
		return false
	}
	return days[1].Month() != today.Month()
}

func fmtWeights(w map[string]float64) string {
	var syms []string
	for s := range w {
		syms = append(syms, s)
	}
	sort.Strings(syms)
	out := ""
	for _, s := range syms {
		out += fmt.Sprintf("%s %.0f%% ", s, w[s]*100)
	}
	return out
}
