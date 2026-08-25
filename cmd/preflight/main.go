// Command preflight runs READ-ONLY safety checks before any live trading:
// startup reconciliation against both venues (positions, open orders, balance).
// It places NO orders. Run this before enabling live mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"predbot/internal/live"
	"predbot/internal/risk"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

func main() {
	// Mirrors the live binary's flag: preflight must reach the SAME verdict the
	// bot will, or it is not a preflight.
	adoptPositions := flag.Bool("adopt-positions", false,
		"accept positions already on the venues instead of refusing")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	kAcct := live.NewKalshiAccount("https://api.elections.kalshi.com/trade-api/v2",
		func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) })
	pAcct := live.NewPolyUSAccount("https://api.polymarket.us",
		func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) })

	eng := risk.New(risk.Config{}, nil, "")
	sup := live.NewSupervisor([]live.AccountClient{kAcct, pAcct}, eng, nil)
	sup.AdoptExisting = *adoptPositions

	fmt.Println("=== predbot preflight: READ-ONLY reconciliation (places nothing) ===")
	if err := sup.Reconcile(ctx); err != nil {
		fmt.Println("RECONCILE FAILED — do not trade:", err)
		os.Exit(1)
	}
	fmt.Println("reconcile OK — both venues readable")
}
