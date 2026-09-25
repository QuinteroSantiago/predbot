// Command testorder verifies the ORDER PATH with minimal/zero risk (Step 1
// before live trading). Modes:
//
//	-mode=preview  (default) Polymarket Preview: validates a real order WITHOUT
//	               executing it. Zero risk.
//	-mode=place    Places ONE real, deliberately NON-MARKETABLE limit order (a
//	               far-from-market price so it rests and cannot fill), then
//	               prints its id so you can verify it in the venue UI, then
//	               cancels it. Requires -i-understand.
//
// The point is to prove market/side/price/size are correct BEFORE any
// marketable order is ever sent.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"predbot/internal/core"
	"predbot/internal/exec"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

func main() {
	mode := flag.String("mode", "preview", "preview | place")
	venueName := flag.String("venue", core.VenuePoly, "polymarketus | kalshi")
	market := flag.String("market", "", "market slug (poly) or ticker (kalshi)")
	priceStr := flag.String("price", "0.0100", "limit price in dollars (keep FAR from market so it cannot fill)")
	contracts := flag.Float64("contracts", 1, "contracts")
	sideStr := flag.String("side", "buy", "buy | sell (YES)")
	confirm := flag.Bool("i-understand", false, "required for -mode=place")
	hold := flag.Bool("hold", false, "leave the order resting instead of cancelling (verify in the UI, then use -mode=cancel)")
	cancelID := flag.String("cancel-id", "", "with -mode=cancel: the order id to cancel")
	flag.Parse()

	if *mode == "cancel" {
		if *cancelID == "" {
			fmt.Fprintln(os.Stderr, "-cancel-id is required with -mode=cancel")
			os.Exit(2)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var err error
		if *venueName == core.VenueKalshi {
			err = exec.NewKalshiExecutor("https://api.elections.kalshi.com/trade-api/v2",
				func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) }, "").Cancel(ctx, *cancelID)
		} else {
			// Polymarket's cancel REQUIRES the market slug in the body; an empty
			// one returns 400. Pass -market alongside -cancel-id.
			if *market == "" {
				fmt.Fprintln(os.Stderr, "-market is required to cancel a polymarketus order")
				os.Exit(2)
			}
			err = exec.NewPolyUSExecutor("https://api.polymarket.us",
				func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) }).
				CancelInMarket(ctx, *cancelID, *market)
		}
		if err != nil {
			fmt.Println("CANCEL FAILED — cancel manually in the UI:", err)
			os.Exit(1)
		}
		fmt.Println("cancelled", *cancelID)
		return
	}

	if *market == "" {
		fmt.Fprintln(os.Stderr, "-market is required")
		os.Exit(2)
	}
	if *mode == "place" && !*confirm {
		fmt.Fprintln(os.Stderr, "REFUSING: -mode=place requires -i-understand")
		os.Exit(2)
	}

	px, err := core.ParsePrice(*priceStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad price:", err)
		os.Exit(2)
	}
	side := core.BuyYes
	if *sideStr == "sell" {
		side = core.SellYes
	}
	o := core.Order{Intent: core.Intent{
		Venue: *venueName, VenueID: *market, Side: side,
		Px: px, Qty: core.Qty(*contracts * 100), TIF: core.GTC, // GTC so it rests, not IOC
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("=== order path test ===\nvenue=%s market=%s side=%s price=%s contracts=%.2f mode=%s\n",
		*venueName, *market, side.String(), px.String(), *contracts, *mode)

	switch *venueName {
	case core.VenuePoly:
		ex := exec.NewPolyUSExecutor("https://api.polymarket.us",
			func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) })
		if *mode == "preview" {
			raw, err := ex.Preview(ctx, o)
			if err != nil {
				fmt.Println("PREVIEW FAILED:", err)
				os.Exit(1)
			}
			fmt.Println("PREVIEW OK (nothing placed):", raw)
			return
		}
		id, err := ex.Place(ctx, o)
		if err != nil {
			fmt.Println("PLACE FAILED:", err)
			os.Exit(1)
		}
		fmt.Println("PLACED resting order id:", id)
		if *hold {
			fmt.Println(">>> order is RESTING. Verify in the Polymarket app/UI, then cancel with:")
			fmt.Printf("    testorder -mode=cancel -venue=polymarketus -cancel-id=%s\n", id)
			return
		}
		fmt.Println(">>> verify it in the Polymarket UI now, then press enter to cancel")
		fmt.Scanln()
		if err := ex.Cancel(ctx, id); err != nil {
			fmt.Println("CANCEL FAILED — cancel it manually in the UI:", err)
			os.Exit(1)
		}
		fmt.Println("cancelled cleanly")

	case core.VenueKalshi:
		ex := exec.NewKalshiExecutor("https://api.elections.kalshi.com/trade-api/v2",
			func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) }, "")
		if *mode == "preview" {
			fmt.Println("kalshi has no preview endpoint — use -mode=place with a far-from-market price")
			return
		}
		id, err := ex.Place(ctx, o)
		if err != nil {
			fmt.Println("PLACE FAILED:", err)
			os.Exit(1)
		}
		fmt.Println("PLACED resting order id:", id)
		if *hold {
			fmt.Println(">>> order is RESTING. Verify in the Kalshi UI, then cancel with:")
			fmt.Printf("    testorder -mode=cancel -venue=kalshi -cancel-id=%s\n", id)
			return
		}
		fmt.Println(">>> verify it in the Kalshi UI now, then press enter to cancel")
		fmt.Scanln()
		if err := ex.Cancel(ctx, id); err != nil {
			fmt.Println("CANCEL FAILED — cancel it manually in the UI:", err)
			os.Exit(1)
		}
		fmt.Println("cancelled cleanly")
	}
}
