// Package obs wires OpenTelemetry to the homelab LGTM stack and defines the
// Phase-1 metric set (design §13). Telemetry is on from line one: feed latency,
// seq gaps, and book age per venue are the Phase-1 dashboard.
package obs

import (
	"context"

	tkobs "tradekit/obs"
)

const serviceName = "predbot"

// Setup installs the OTLP providers under the "predbot" service name; see
// tradekit/obs. Returns a shutdown func.
func Setup(ctx context.Context) func(context.Context) error {
	return tkobs.Setup(ctx, serviceName)
}
