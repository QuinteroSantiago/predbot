// Command filltest isolates the Polymarket US private fill stream.
//
// It answers one question the trading loop cannot: when an order fills, does an
// execution event actually arrive on the WebSocket? It logs EVERY raw frame
// verbatim, because the production reader silently drops frames it cannot parse
// (fills_poly.go), so "no fills" and "fills in an unexpected shape" look
// identical from the outside.
//
//	-endpoint private|markets   which socket to subscribe on (default private)
//	-listen-only                connect and listen; place NO order (zero risk)
//	-market <slug>              market to subscribe to (and trade, if placing)
//	-side buy|sell -contracts N -price P   the order to place
//	-i-understand               required before any real order is sent
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/live"
	"predbot/internal/venue"
	"predbot/internal/venue/polymarketus"
)

func main() {
	endpoint := flag.String("endpoint", "private", "private | markets")
	listenOnly := flag.Bool("listen-only", false, "connect and listen only; place no order")
	market := flag.String("market", "", "market slug")
	side := flag.String("side", "sell", "buy | sell (YES)")
	contracts := flag.Float64("contracts", 1, "contracts")
	price := flag.String("price", "", "limit price in dollars, e.g. 0.0900")
	wait := flag.Duration("wait", 25*time.Second, "how long to listen after placing")
	confirm := flag.Bool("i-understand", false, "required to place a real order")
	tif := flag.String("tif", "ioc", "ioc | gtc (gtc rests so you can test 'new' order updates)")
	subtypes := flag.String("subtypes", "SUBSCRIPTION_TYPE_ORDER", "comma-separated subscriptionType values to subscribe to")
	allMarkets := flag.Bool("all-markets", false, "omit marketSlugs (docs claim this means ALL markets)")
	flag.Parse()

	if *market == "" {
		fmt.Fprintln(os.Stderr, "-market is required")
		os.Exit(2)
	}
	path := "/v1/ws/private"
	if *endpoint == "markets" {
		path = "/v1/ws/markets"
	}
	wsURL := "wss://api.polymarket.us" + path

	ctx, cancel := context.WithTimeout(context.Background(), *wait+60*time.Second)
	defer cancel()

	fmt.Printf("=== dialing %s\n", wsURL)
	h, err := polymarketus.AuthHeaders("GET", path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth:", err)
		os.Exit(1)
	}
	conn, err := venue.DialWS(wsURL, h)(ctx)
	if err != nil {
		fmt.Printf("!!! DIAL FAILED: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("=== connected")

	for i, st := range strings.Split(*subtypes, ",") {
		st = strings.TrimSpace(st)
		if st == "" {
			continue
		}
		body := map[string]any{
			"requestId":        fmt.Sprintf("filltest-%d", i),
			"subscriptionType": st,
		}
		// Docs say an empty marketSlugs subscribes to ALL markets; the production
		// code found the opposite in 2026-08. -all-markets tests that claim.
		if !*allMarkets {
			body["marketSlugs"] = []string{*market}
		}
		sub, _ := json.Marshal(map[string]any{"subscribe": body})
		if err := conn.Write(ctx, sub); err != nil {
			fmt.Printf("!!! subscribe write failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("=== sent subscribe: %s\n", string(sub))
	}

	// Every frame, verbatim. This is the whole point of the tool.
	frames := make(chan string, 256)
	go func() {
		for {
			raw, err := conn.Read(ctx)
			if err != nil {
				close(frames)
				return
			}
			frames <- string(raw)
		}
	}()

	done := make(chan struct{})
	var total, execs int
	go func() {
		defer close(done)
		for f := range frames {
			total++
			tag := "     "
			if strings.Contains(f, "execution") {
				tag = "EXEC "
				execs++
			}
			if len(f) > 100000 {
				f = f[:100000] + "…(truncated)"
			}
			fmt.Printf("[%s] %s%s\n", time.Now().Format("15:04:05.000"), tag, f)
		}
	}()

	if *listenOnly {
		fmt.Printf("=== listen-only: no order will be placed; listening %s\n", *wait)
		time.Sleep(*wait)
	} else {
		if !*confirm {
			fmt.Fprintln(os.Stderr, "refusing to place a real order without -i-understand")
			os.Exit(2)
		}
		if *price == "" {
			fmt.Fprintln(os.Stderr, "-price is required when placing")
			os.Exit(2)
		}
		px, err := core.ParsePrice(*price)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad -price:", err)
			os.Exit(2)
		}
		qty := core.Qty(int64(*contracts * 100))
		tifVal := core.IOC
		if strings.EqualFold(*tif, "gtc") {
			tifVal = core.GTC
		}
		s := core.SellYes
		if *side == "buy" {
			s = core.BuyYes
		}
		acct := live.NewPolyUSAccount("https://api.polymarket.us",
			func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) })
		before, _ := acct.Positions(ctx)
		fmt.Printf("=== REST position BEFORE: %+v\n", before)

		time.Sleep(2 * time.Second) // let the subscription settle
		ex := exec.NewPolyUSExecutor("https://api.polymarket.us",
			func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) })
		fmt.Printf("=== placing REAL order: %s %s %.0f contracts @ %s\n", *side, *market, *contracts, *price)
		id, err := ex.Place(ctx, core.Order{Intent: core.Intent{
			Venue: "polymarketus", VenueID: *market, Side: s, Px: px, Qty: qty, TIF: tifVal,
		}})
		if err != nil {
			fmt.Printf("!!! PLACE FAILED: %v\n", err)
		} else {
			fmt.Printf("=== placed, venue_order_id=%s\n", id)
		}
		fmt.Printf("=== listening %s for an execution event…\n", *wait)
		time.Sleep(*wait)
		after, _ := acct.Positions(ctx)
		fmt.Printf("=== REST position AFTER: %+v\n", after)
	}

	conn.Close()
	<-done
	fmt.Printf("\n=== VERDICT: %d raw frame(s), %d containing an execution\n", total, execs)
	if total == 0 {
		fmt.Println("    ZERO frames — the socket delivered nothing at all.")
	} else if execs == 0 {
		fmt.Println("    Frames arrived but NONE were executions.")
	}
}
