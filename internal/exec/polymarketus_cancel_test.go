package exec

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"predbot/internal/core"
)

// Until 2026-09-24 Cancel sent an empty marketSlug and every cancel was
// rejected. It must send the slug the order was placed in.
func TestPolyCancelSendsPlacedSlug(t *testing.T) {
	var cancelBody map[string]string
	var cancelPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/orders":
			io.WriteString(w, `{"id":"OID1"}`)
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelPath = r.URL.Path
			json.NewDecoder(r.Body).Decode(&cancelBody)
			io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ex := NewPolyUSExecutor(srv.URL, func(string, string) (http.Header, error) { return http.Header{}, nil })

	id, err := ex.Place(context.Background(), core.Order{Intent: core.Intent{
		Venue: "polymarketus", VenueID: "aec-mlb-stl-pit-2026-09-24", Side: core.SellYes, Px: 4350, Qty: 2400, TIF: core.GTC}})
	if err != nil || id != "OID1" {
		t.Fatalf("place = %q, %v", id, err)
	}
	if err := ex.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelPath != "/v1/order/OID1/cancel" || cancelBody["marketSlug"] != "aec-mlb-stl-pit-2026-09-24" {
		t.Errorf("cancel sent %s %v; want the placed slug", cancelPath, cancelBody)
	}
}

// An id this executor never placed has no known market: refuse rather than
// send the empty slug the venue rejects.
func TestPolyCancelUnknownOrderRefuses(t *testing.T) {
	ex := NewPolyUSExecutor("http://127.0.0.1:1", func(string, string) (http.Header, error) { return http.Header{}, nil })
	if err := ex.Cancel(context.Background(), "NOPE"); err == nil {
		t.Fatal("cancel of an unknown order must error, not send an empty slug")
	}
}
