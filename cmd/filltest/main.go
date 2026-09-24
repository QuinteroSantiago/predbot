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
//
// Reproducing the live trader's subscription (the 2026-09-24 silent fills
// happened with ~290 slugs sent as three 100-slug ORDER subscriptions):
//
//	-pad N -pad-leagues mlb,nfl  also subscribe N other real slugs, chunked by
//	                            100 with the trader's requestIds (order-sub-0..)
//	-target-chunk first|last    which chunk the traded market goes in. If only
//	                            the LAST subscription sticks, "first" goes silent
//	-resubscribe-after D        resend every chunk after D, as the trader does
//	                            every 2 min (tests whether a resend drops them)
//	-exit-price P               after the wait, place the opposite IOC at P and
//	                            listen again — a round trip that ends flat
//
// Each placed order also reports how long the REST position took to reflect
// the fill (polled every -poll), next to the order ack and the first execution
// event, as a LATENCY line. The trader checks REST positions 1s after an IOC
// that delivered no fill event, so this says whether 1s is enough.
//
// Round trip on one contract, costing about the spread:
//
//	filltest -market <slug> -side buy -price 0.99 -exit-price 0.01 -tif ioc \
//	  -pad 250 -pad-leagues mlb,nfl,wnba,cfb,nhl -target-chunk first -i-understand
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/live"
	"predbot/internal/sports"
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
	pad := flag.Int("pad", 0, "also subscribe this many other real slugs, chunked by 100 like the trader")
	padLeagues := flag.String("pad-leagues", "mlb,nfl,wnba,cfb,nhl", "leagues to draw -pad slugs from")
	targetChunk := flag.String("target-chunk", "first", "first | last: which 100-slug chunk holds -market")
	resubAfter := flag.Duration("resubscribe-after", 0, "resend every subscription after this long (the trader resends every 2m); 0 = never")
	exitPrice := flag.String("exit-price", "", "after the wait, place the opposite side IOC at this price (round trip)")
	poll := flag.Duration("poll", 100*time.Millisecond, "how often to poll the REST position after placing, to time when it reflects the fill")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2**wait+*resubAfter+2*time.Minute)
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

	slugs := []string{*market}
	if *pad > 0 {
		others := padSlugs(ctx, *padLeagues, *market, *pad)
		if *targetChunk == "last" {
			slugs = append(others, *market)
		} else {
			slugs = append([]string{*market}, others...)
		}
		fmt.Printf("=== %d slugs in %d chunk(s); -market is in the %s chunk\n", len(slugs), (len(slugs)+99)/100, *targetChunk)
	}
	subscribe := func() {
		for i, st := range strings.Split(*subtypes, ",") {
			st = strings.TrimSpace(st)
			if st == "" {
				continue
			}
			// Docs say an empty marketSlugs subscribes to ALL markets; the production
			// code found the opposite in 2026-08. -all-markets tests that claim.
			if *allMarkets {
				send(ctx, conn, map[string]any{"requestId": fmt.Sprintf("filltest-%d", i), "subscriptionType": st})
				continue
			}
			for c := 0; c < len(slugs); c += 100 {
				id := fmt.Sprintf("filltest-%d", i)
				if st == "SUBSCRIPTION_TYPE_ORDER" {
					id = fmt.Sprintf("order-sub-%d", c/100) // the trader's ids
				} else if c > 0 {
					id = fmt.Sprintf("filltest-%d-%d", i, c/100)
				}
				send(ctx, conn, map[string]any{"requestId": id, "subscriptionType": st,
					"marketSlugs": slugs[c:min(c+100, len(slugs))]})
			}
		}
	}
	subscribe()
	if *resubAfter > 0 {
		go func() {
			time.Sleep(*resubAfter)
			fmt.Printf("[%s] === RESUBSCRIBING (as the trader does every 2m)\n", time.Now().Format("15:04:05.000"))
			subscribe()
		}()
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
	var total, execs, targetExecs int
	var execMu sync.Mutex
	var execTimes []time.Time // arrival of each execution frame on -market
	go func() {
		defer close(done)
		for f := range frames {
			total++
			tag := "     "
			if strings.Contains(f, "execution") {
				tag = "EXEC "
				execs++
				if strings.Contains(f, *market) {
					targetExecs++
					execMu.Lock()
					execTimes = append(execTimes, time.Now())
					execMu.Unlock()
				}
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
		firstExecAfter := func(t0 time.Time) time.Duration {
			execMu.Lock()
			defer execMu.Unlock()
			for _, at := range execTimes {
				if !at.Before(t0) {
					return at.Sub(t0)
				}
			}
			return -1
		}
		// timeFill places o, polls the REST position until it moves off `from`,
		// then waits out -wait, and prints one LATENCY line.
		timeFill := func(label string, o core.Order, from float64) {
			t0 := time.Now()
			id, err := ex.Place(ctx, o)
			ack := time.Since(t0)
			if err != nil {
				fmt.Printf("!!! %s PLACE FAILED: %v — check the position by hand\n", label, err)
				return
			}
			fmt.Printf("=== %s placed, venue_order_id=%s (ack %s)\n", label, id, ack.Round(time.Millisecond))
			rest, polls := time.Duration(-1), 0
			for time.Since(t0) < *wait {
				pos, err := acct.Positions(ctx)
				polls++
				if err == nil && pos[*market] != from {
					rest = time.Since(t0)
					break
				}
				time.Sleep(*poll)
			}
			if left := *wait - time.Since(t0); left > 0 {
				time.Sleep(left) // keep listening for late execution frames
			}
			show := func(d time.Duration) string {
				if d < 0 {
					return "never (within -wait)"
				}
				return "+" + d.Round(time.Millisecond).String()
			}
			fmt.Printf("=== LATENCY %s: ack %s, REST position moved %s after %d poll(s), first exec event %s\n",
				label, show(ack), show(rest), polls, show(firstExecAfter(t0)))
		}

		fmt.Printf("=== placing REAL order: %s %s %.0f contracts @ %s\n", *side, *market, *contracts, *price)
		timeFill("entry", core.Order{Intent: core.Intent{
			Venue: "polymarketus", VenueID: *market, Side: s, Px: px, Qty: qty, TIF: tifVal,
		}}, before[*market])
		after, _ := acct.Positions(ctx)
		fmt.Printf("=== REST position AFTER: %+v\n", after)

		if *exitPrice != "" {
			xpx, err := core.ParsePrice(*exitPrice)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bad -exit-price:", err)
				os.Exit(2)
			}
			xs := core.BuyYes
			if s == core.BuyYes {
				xs = core.SellYes
			}
			fmt.Printf("=== placing REAL exit: %s %s %.0f contracts @ %s IOC\n", xs, *market, *contracts, *exitPrice)
			timeFill("exit", core.Order{Intent: core.Intent{
				Venue: "polymarketus", VenueID: *market, Side: xs, Px: xpx, Qty: qty, TIF: core.IOC,
			}}, after[*market])
			final, _ := acct.Positions(ctx)
			fmt.Printf("=== REST position FINAL: %+v\n", final)
		}
	}

	conn.Close()
	<-done
	fmt.Printf("\n=== VERDICT: %d raw frame(s), %d containing an execution (%d on -market)\n", total, execs, targetExecs)
	if total == 0 {
		fmt.Println("    ZERO frames — the socket delivered nothing at all.")
	} else if execs == 0 {
		fmt.Println("    Frames arrived but NONE were executions.")
	}
}

func send(ctx context.Context, conn venue.Conn, body map[string]any) {
	sub, _ := json.Marshal(map[string]any{"subscribe": body})
	if err := conn.Write(ctx, sub); err != nil {
		fmt.Printf("!!! subscribe write failed: %v\n", err)
		os.Exit(1)
	}
	shown := string(sub)
	if len(shown) > 300 {
		shown = shown[:300] + "…"
	}
	fmt.Printf("=== sent subscribe: %s\n", shown)
}

// padSlugs returns up to n real open Poly slugs other than target, so the
// subscription looks like the trader's.
func padSlugs(ctx context.Context, leagues, target string, n int) []string {
	c := sports.New()
	var out []string
	for _, lg := range strings.Split(leagues, ",") {
		slugs, err := c.PolySlugs(ctx, strings.TrimSpace(lg))
		if err != nil {
			fmt.Printf("!!! discovering %s: %v\n", lg, err)
			continue
		}
		for _, s := range slugs {
			if s != target && len(out) < n {
				out = append(out, s)
			}
		}
	}
	if len(out) < n {
		fmt.Printf("=== only found %d pad slugs (wanted %d)\n", len(out), n)
	}
	return out
}
