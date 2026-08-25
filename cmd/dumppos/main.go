// dumppos prints the RAW positions JSON from both venues alongside what the
// production parser makes of it.
//
// It exists because a GUESSED field name ("quantity" instead of "netPosition")
// silently made every Polymarket position parse as zero, which disabled both the
// drift monitor and the silent-fill verifier and let the bot sit on two naked
// shorts on 2026-08-25. Raw-vs-parsed side by side is the check that catches
// that: if the venue changes shape, PARSED goes empty while RAW does not.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"

	"predbot/internal/live"
	"predbot/internal/venue/kalshi"
	"predbot/internal/venue/polymarketus"
)

func raw(name, url string, hdr http.Header) {
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "predbot/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("=== %s RAW: ERROR %v\n", name, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("=== %s RAW (HTTP %d)\n%s\n", name, resp.StatusCode, string(b))
}

func parsed(name string, a live.AccountClient) {
	pos, err := a.Positions(context.Background())
	if err != nil {
		fmt.Printf("--- %s PARSED: ERROR %v\n\n", name, err)
		return
	}
	if len(pos) == 0 {
		fmt.Printf("--- %s PARSED: (flat)\n\n", name)
		return
	}
	keys := make([]string, 0, len(pos))
	for k := range pos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("--- %s PARSED: %d position(s)\n", name, len(pos))
	for _, k := range keys {
		fmt.Printf("      %-40s %+.2f\n", k, pos[k])
	}
	fmt.Println()
}

func main() {
	pSign := func(m, p string) (http.Header, error) { return polymarketus.AuthHeaders(m, p) }
	kSign := func(m, p string) (http.Header, error) { return kalshi.AuthHeaders(m, "/trade-api/v2"+p) }

	ph, err := pSign("GET", "/v1/portfolio/positions")
	if err != nil {
		fmt.Println("poly auth:", err)
		os.Exit(1)
	}
	raw("POLY /v1/portfolio/positions", "https://api.polymarket.us/v1/portfolio/positions", ph)
	parsed("POLY", live.NewPolyUSAccount("https://api.polymarket.us", pSign))

	kh, err := kSign("GET", "/portfolio/positions")
	if err != nil {
		fmt.Println("kalshi auth:", err)
		os.Exit(1)
	}
	raw("KALSHI /portfolio/positions", "https://api.elections.kalshi.com/trade-api/v2/portfolio/positions", kh)
	parsed("KALSHI", live.NewKalshiAccount("https://api.elections.kalshi.com/trade-api/v2", kSign))
}
