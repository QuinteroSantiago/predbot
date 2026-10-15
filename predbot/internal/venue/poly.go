package venue

// PolyMarketsPerSubscription is Polymarket US's documented cap on market slugs
// in one WebSocket subscription; more markets need several subscriptions on
// the same connection (docs.polymarket.us, private WebSocket "Subscription
// Limits"; verified with >100 slugs on 2026-09-24).
const PolyMarketsPerSubscription = 100

// ChunkSlugs splits slugs into subscription-sized chunks, in order. Every
// chunk must be sent: on Poly an empty slug list subscribes to nothing.
func ChunkSlugs(slugs []string) [][]string {
	var out [][]string
	for i := 0; i < len(slugs); i += PolyMarketsPerSubscription {
		out = append(out, slugs[i:min(i+PolyMarketsPerSubscription, len(slugs))])
	}
	return out
}
