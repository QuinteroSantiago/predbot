// Command cancelopen lists the account's open Polymarket US orders and, with
// -yes, cancels all of them, then lists again to confirm the book is clear.
//
// The bot only ever RESTS on Polymarket (maker mode), so this is where orphaned
// resting orders live. Stopping the bot does not cancel them, and until
// 2026-09-24 the bot's own cancels all failed, so rests it had "abandoned"
// stayed working and filled later with no hedge.
//
//	cancelopen          list open orders (read-only)
//	cancelopen -yes     cancel every open order, then re-list
//
// Exits 1 if orders remain open afterwards (or could not be listed).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"predbot/internal/exec"
	"predbot/internal/venue/polymarketus"
)

const base = "https://api.polymarket.us"

type openOrder struct {
	ID         string          `json:"id"`
	MarketSlug string          `json:"marketSlug"`
	Side       string          `json:"side"`
	Intent     string          `json:"intent"`
	Price      json.RawMessage `json:"price"`
	Quantity   json.RawMessage `json:"quantity"`
	State      string          `json:"state"`
}

func main() {
	yes := flag.Bool("yes", false, "cancel every open order (default: list only)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orders, err := listOpen(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list open orders:", err)
		os.Exit(1)
	}
	show(orders)
	if len(orders) == 0 || !*yes {
		if len(orders) > 0 {
			fmt.Println("(list only — rerun with -yes to cancel them)")
		}
		return
	}

	ex := exec.NewPolyUSExecutor(base, func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) })
	ids, err := ex.CancelAllOpen(ctx, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cancel all:", err)
	} else {
		fmt.Printf("cancel-all request accepted for %d order(s)\n", len(ids))
	}
	time.Sleep(time.Second) // the response echoes the request; confirm from the book
	left, err := listOpen(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "re-list open orders:", err)
		os.Exit(1)
	}
	if len(left) > 0 {
		fmt.Printf("STILL OPEN after cancel-all — cancel these in the Polymarket app:\n")
		show(left)
		os.Exit(1)
	}
	fmt.Println("no open orders remain on Polymarket US")
}

func listOpen(ctx context.Context) ([]openOrder, error) {
	const path = "/v1/orders/open"
	hdr, err := polymarketus.AuthHeaders(http.MethodGet, path)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "Mozilla/5.0") // the WAF 403s Go's default agent
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, b)
	}
	var out struct {
		Orders []openOrder `json:"orders"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out.Orders, nil
}

func show(orders []openOrder) {
	fmt.Printf("%d open order(s) on Polymarket US\n", len(orders))
	for _, o := range orders {
		fmt.Printf("  %s  %-40s %s %s px=%s qty=%s %s\n", o.ID, o.MarketSlug, o.Intent, o.Side, o.Price, o.Quantity, o.State)
	}
}
