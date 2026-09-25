package exec

import (
	"context"
	"time"

	"predbot/internal/core"
)

// Executor places and cancels real orders at a venue (design §11). The paper
// SimExecutor is NOT an Executor — it never talks to a venue. A real Executor is
// used only from Phase 3b (Kalshi demo, fake money) onward, always behind the
// risk gate (internal/risk).
// orderHTTPTimeout bounds one order-entry request to a venue. A timeout means
// "unknown", not "failed": the order may have landed (see Place).
const orderHTTPTimeout = 5 * time.Second

type Executor interface {
	// Name identifies the venue (and environment, e.g. "kalshi-demo").
	Name() string
	// Place submits an order and returns the venue's order id. On a timeout or
	// 5xx it MUST return an error WITHOUT resubmitting — the caller reconciles by
	// client id (design §11: a retried submit without reconcile is how positions
	// double).
	Place(ctx context.Context, o core.Order) (venueOrderID string, err error)
	// Cancel cancels a resting order by its venue id.
	Cancel(ctx context.Context, venueOrderID string) error
}

// NopExecutor is the safe default: it accepts nothing and errors on Place, so a
// misconfigured build cannot accidentally trade.
type NopExecutor struct{}

func (NopExecutor) Name() string { return "nop" }
func (NopExecutor) Place(context.Context, core.Order) (string, error) {
	return "", errNotWired
}
func (NopExecutor) Cancel(context.Context, string) error { return errNotWired }

var errNotWired = errNotWiredType{}

type errNotWiredType struct{}

func (errNotWiredType) Error() string {
	return "exec: no real executor wired — order placement is disabled by default"
}
