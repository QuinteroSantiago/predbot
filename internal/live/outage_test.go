package live

import (
	"context"
	"errors"
	"testing"
	"time"

	"predbot/internal/risk"
)

// The error shape Polymarket US returned for 1h40m on 2026-09-11.
var maintenance503 = &HTTPError{Venue: "polymarketus", Path: "/v1/portfolio/positions", Status: 503,
	Body: `{"code":14, "message":"The server was unable to process your request.", "details":[]}`}

func fakeClock(o *Outage) *time.Time {
	t := time.Date(2026, 9, 11, 8, 47, 0, 0, time.UTC)
	o.now = func() time.Time { return t }
	return &t
}

// A venue is only DOWN after Threshold CONSECUTIVE 5xx; one transient 503 (or a
// streak broken by a success) must not silence it.
func TestOutageTripsOnlyOnConsecutive5xx(t *testing.T) {
	o := NewOutage("polymarketus")
	fakeClock(o)
	o.Observe(maintenance503)
	o.Observe(maintenance503)
	o.Observe(nil) // streak broken
	o.Observe(maintenance503)
	o.Observe(maintenance503)
	if o.Down() {
		t.Fatal("no run of 3 consecutive 5xx yet — must not be down")
	}
	o.Observe(maintenance503)
	if !o.Down() {
		t.Fatal("3 consecutive 5xx must mark the venue down")
	}
}

// 4xx is our bug and a transport error may be our own network — neither is the
// venue being in maintenance, so neither may trip the breaker.
func TestOutageIgnoresNon5xx(t *testing.T) {
	o := NewOutage("polymarketus")
	fakeClock(o)
	for i := 0; i < 10; i++ {
		o.Observe(&HTTPError{Venue: "polymarketus", Status: 401})
		o.Observe(errors.New("context deadline exceeded"))
	}
	if o.Down() {
		t.Fatal("4xx / transport errors must not mark a venue down")
	}
}

// While DOWN, exactly one caller per Probe window gets through, however many
// pollers share the breaker; a successful probe restores normal service.
func TestOutageProbesOncePerWindowThenRecovers(t *testing.T) {
	o := NewOutage("polymarketus")
	now := fakeClock(o)
	for i := 0; i < 3; i++ {
		o.Observe(maintenance503)
	}
	if o.Allow() {
		t.Fatal("just went down — no probe is due yet")
	}
	*now = now.Add(15 * time.Minute)
	if !o.Allow() {
		t.Fatal("a probe is due after the Probe interval")
	}
	if o.Allow() {
		t.Fatal("only ONE caller may probe per window")
	}
	o.Observe(maintenance503) // probe failed: still down, next window
	if !o.Down() || o.Allow() {
		t.Fatal("a failed probe keeps the venue down until the next window")
	}
	*now = now.Add(15 * time.Minute)
	if !o.Allow() {
		t.Fatal("next window's probe must be admitted")
	}
	o.Observe(nil)
	if o.Down() || !o.Allow() || !o.Allow() {
		t.Fatal("a successful probe must restore normal, unthrottled service")
	}
}

type flakyAcct struct {
	fakeAcct
	calls int
	err   error
}

func (c *flakyAcct) Positions(ctx context.Context) (map[string]float64, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.fakeAcct.Positions(ctx)
}

// A GuardedAccount must not call a DOWN venue between probes, and Unguarded
// (the silent-fill verifier's path) must always reach it.
func TestGuardedAccountSkipsDownVenue(t *testing.T) {
	inner := &flakyAcct{fakeAcct: fakeAcct{venue: "polymarketus"}, err: maintenance503}
	o := NewOutage("polymarketus")
	fakeClock(o)
	g := Guard(inner, o)
	for i := 0; i < 3; i++ {
		if _, err := g.Positions(context.Background()); !errors.Is(err, maintenance503) {
			t.Fatalf("call %d: want the venue's own error, got %v", i, err)
		}
	}
	for i := 0; i < 50; i++ {
		if _, err := g.Positions(context.Background()); !errors.Is(err, ErrVenueUnavailable) {
			t.Fatalf("down venue: want ErrVenueUnavailable, got %v", err)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("down venue was called %d times, want 3 (the calls that tripped it)", inner.calls)
	}
	if _, err := g.Unguarded().Positions(context.Background()); err == nil || inner.calls != 4 {
		t.Fatal("Unguarded must bypass the breaker and hit the venue")
	}
}

// A venue in maintenance must not blind the drift monitor to the OTHER venue:
// it skips the down venue and still reconciles (and kills on) the healthy one.
func TestDriftSkipsVenueInMaintenance(t *testing.T) {
	eng := risk.New(risk.Config{}, nil, "")
	o := NewOutage("polymarketus")
	fakeClock(o)
	down := Guard(&flakyAcct{fakeAcct: fakeAcct{venue: "polymarketus"}, err: maintenance503}, o)
	for i := 0; i < 3; i++ {
		down.Positions(context.Background())
	}
	up := &fakeAcct{venue: "kalshi", pos: map[string]float64{"K": 5}} // unexplained → must still kill
	d := NewDriftMonitor([]AccountClient{down, up}, eng, func() map[string]float64 { return nil })
	d.check(context.Background())
	if !eng.Killed() {
		t.Fatal("drift on the healthy venue must still kill while the other is in maintenance")
	}
}
