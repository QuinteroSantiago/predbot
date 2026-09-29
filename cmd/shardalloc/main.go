// Command shardalloc reads, and with -set replaces, the Kalshi target balance
// allocation across exchange shards. Kalshi funds are per shard (baseball and
// basketball trade on shard 3, most other sports on 0), and with an allocation
// set Kalshi rebalances the account toward it automatically.
//
//	shardalloc                  print allocation + balance by shard
//	shardalloc -set 0:50,3:50   replace the allocation (percents must sum to 100)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"predbot/internal/config"
	"predbot/internal/venue/kalshi"
)

const requestTimeout = 15 * time.Second

func call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	hdr, err := kalshi.AuthHeaders(method, "/trade-api/v2"+path)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, method, config.KalshiAPI+path, rd)
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, out)
	}
	return out, nil
}

func show(ctx context.Context) error {
	a, err := call(ctx, http.MethodGet, "/portfolio/target_balance_allocation", nil)
	if err != nil {
		return err
	}
	b, err := call(ctx, http.MethodGet, "/portfolio/balance", nil)
	if err != nil {
		return err
	}
	var bal struct {
		Breakdown json.RawMessage `json:"balance_breakdown"`
	}
	_ = json.Unmarshal(b, &bal)
	fmt.Printf("allocation: %s\nbalance by shard: %s\n", bytes.TrimSpace(a), bal.Breakdown)
	return nil
}

func main() {
	set := flag.String("set", "", "shard:percent pairs, e.g. 0:50,3:50")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	if *set != "" {
		type alloc struct {
			ExchangeIndex int `json:"exchange_index"`
			Percent       int `json:"percent"`
		}
		var allocs []alloc
		total := 0
		for _, p := range strings.Split(*set, ",") {
			sh, pct, ok := strings.Cut(p, ":")
			s, e1 := strconv.Atoi(sh)
			n, e2 := strconv.Atoi(pct)
			if !ok || e1 != nil || e2 != nil {
				fmt.Fprintf(os.Stderr, "bad pair %q (want shard:percent)\n", p)
				os.Exit(2)
			}
			allocs = append(allocs, alloc{s, n})
			total += n
		}
		if total != 100 {
			fmt.Fprintf(os.Stderr, "percents sum to %d, must be 100\n", total)
			os.Exit(2)
		}
		fmt.Println("before:")
		if err := show(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := call(ctx, http.MethodPost, "/portfolio/target_balance_allocation",
			map[string]any{"allocations": allocs}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("after:")
	}
	if err := show(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
