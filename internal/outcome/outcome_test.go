package outcome
import ("context";"testing";"time")
func TestFetchKalshiSettled(t *testing.T) {
  ctx,cancel := context.WithTimeout(context.Background(), 20*time.Second); defer cancel()
  // a real finalized market found on 2026-08-24
  r, ok, err := FetchKalshi(ctx, "https://api.elections.kalshi.com/trade-api/v2",
    "KXMVECROSSCATEGORY-SHARD1-S20269CEE8AEABAD-7D56C07DB44")
  if err != nil { t.Skip("network:", err) }
  if !ok { t.Fatal("expected a settled result") }
  if r.Outcome != "yes" && r.Outcome != "no" { t.Errorf("outcome=%q", r.Outcome) }
  t.Logf("OK: %s -> outcome=%s status=%s", r.VenueID, r.Outcome, r.Status)
}
func TestFetchKalshiUnsettled(t *testing.T) {
  ctx,cancel := context.WithTimeout(context.Background(), 20*time.Second); defer cancel()
  _, ok, err := FetchKalshi(ctx, "https://api.elections.kalshi.com/trade-api/v2", "CONTROLH-2026-D")
  if err != nil { t.Skip("network:", err) }
  if ok { t.Error("an open market must NOT report a settled outcome") }
}
