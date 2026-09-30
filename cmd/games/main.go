// Command games records live MLB game events beside the market data, for price
// studies: schedule and status, in-game state, every completed play, and MLB's
// per-at-bat win probability, to <data>/games/mlb-<YYYY-MM-DDTHH>.jsonl.gz.
//
// It reads MLB's public Stats API (no key) and never touches a venue or the
// live trader. Run it on the Pi beside the backup, as predbot-games.service:
//
//	games -data /home/santiago/backups/predbot/data
//	games -data DIR -backfill 2026-08-22:2026-09-22   # finished games, then exit
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"predbot/internal/games"
	"tradekit/journal"
)

func main() {
	dataDir := flag.String("data", "", "data directory; records go to <data>/games/ (required)")
	poll := flag.Duration("poll", 10*time.Second, "how often to poll live games")
	backfill := flag.String("backfill", "", "record finished games for FROM:TO (YYYY-MM-DD, inclusive), then exit")
	runFor := flag.Duration("for", 0, "stop after this long (0 = run until signalled); for testing")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *dataDir == "" {
		log.Error("-data is required")
		os.Exit(2)
	}
	out, err := journal.NewStream(*dataDir, "games")
	if err != nil {
		log.Error("open games stream failed", "err", err)
		os.Exit(1)
	}
	defer out.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *runFor)
		defer cancel()
	}

	c := games.NewCollector(games.NewMLB(), out, log)
	n, err := c.Resume(filepath.Join(*dataDir, "games"))
	if err != nil {
		log.Error("reading existing records failed", "err", err)
		os.Exit(1)
	}
	log.Info("resumed", "records_already_written", n)
	if *backfill != "" {
		c.Prefix = "mlb-backfill" // never the live service's file
		from, to, ok := strings.Cut(*backfill, ":")
		if !ok {
			to = from
		}
		if err := c.Backfill(ctx, from, to); err != nil {
			log.Error("backfill failed", "err", err)
			os.Exit(1)
		}
		return
	}

	log.Info("recording MLB game events", "dir", *dataDir+"/games", "poll", *poll)
	t := time.NewTicker(*poll)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return
		case <-t.C:
		}
	}
}
