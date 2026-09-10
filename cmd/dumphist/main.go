// dumphist prints raw settlement / fill / order history from both venues, to
// reconcile actual exchange balance changes against the bot's internal
// RealizedUSD (which only counts round-trips the bot itself completed). Adopted
// positions, legged exits and manual closes show up here but not in the bot log.
// Read-only: issues only GETs, places nothing.
package main

import (
	"fmt"
	"io"
	"net/http"

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
		fmt.Printf("=== %s ERROR %v\n\n", name, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("=== %s (HTTP %d)\n%s\n\n", name, resp.StatusCode, string(b))
}

// query string is NOT part of the signed path (both venues sign path only).
func kGET(path, query string) {
	h, err := kalshi.AuthHeaders("GET", "/trade-api/v2"+path)
	if err != nil {
		fmt.Println("kalshi auth:", err)
		return
	}
	raw("KALSHI "+path, "https://api.elections.kalshi.com/trade-api/v2"+path+query, h)
}

func pGET(path, query string) {
	h, err := polymarketus.AuthHeaders("GET", path)
	if err != nil {
		fmt.Println("poly auth:", err)
		return
	}
	raw("POLY "+path, "https://api.polymarket.us"+path+query, h)
}

func main() {
	// Kalshi: settlements = realized P&L on resolved markets; fills = executions.
	kGET("/portfolio/settlements", "?limit=500")
	kGET("/portfolio/fills", "?limit=500")

	// Polymarket US: current positions plus candidate history endpoints
	// (unknown ones will 404/501 — we print whatever answers).
	pGET("/v1/portfolio/positions", "")
	pGET("/v1/portfolio/settlements", "?limit=500")
	pGET("/v1/portfolio/history", "")
	pGET("/v1/fills", "?limit=500")
	pGET("/v1/orders", "?status=filled&limit=500")
	pGET("/v1/orders/history", "?limit=500")
}
