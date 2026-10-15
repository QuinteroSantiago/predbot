package live

import "time"

// Timeouts and intervals of the live path. Tuning an operator might change is
// config.yaml's live: block (config.Live); these are engineering choices.
const (
	// orderTimeout bounds placing, cancelling or checking one order. A
	// placement that runs past it is NOT assumed to have failed — the order
	// may have landed — so callers reconcile against the venue (design §11).
	orderTimeout = 5 * time.Second
	// accountHTTPTimeout bounds one account read: positions, balance, open orders.
	accountHTTPTimeout = 8 * time.Second
	// positionReadTimeout bounds the drift monitor's read of one venue.
	positionReadTimeout = 15 * time.Second
	// polyResubscribeEvery re-sends the Poly fill-stream subscriptions so
	// newly discovered markets are covered even without an explicit poke.
	polyResubscribeEvery = 2 * time.Minute
	// killCheckEvery is how often the supervisor evaluates its kill conditions.
	killCheckEvery = 5 * time.Second
	// cancelRaceDrain is how long to keep collecting fill events after a
	// rest's cancel, for fills that landed just before the venue acked it.
	cancelRaceDrain = 500 * time.Millisecond
	// cancelRetryDelay spaces the retries of a failed cancel.
	cancelRetryDelay = 500 * time.Millisecond
)
