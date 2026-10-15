package venue

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// wsConn adapts *websocket.Conn to the Conn interface.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) ([]byte, error) {
	_, b, err := w.c.Read(ctx)
	return b, err
}

func (w wsConn) Write(ctx context.Context, b []byte) error {
	return w.c.Write(ctx, websocket.MessageText, b)
}

func (w wsConn) Close() error { return w.c.Close(websocket.StatusNormalClosure, "") }

// DialWS returns a Dialer that connects to url with the given headers. Callers
// build the auth headers (which is where venue credentials enter) and pass them
// here; if headers is nil the dial still proceeds, so venue-specific dialers are
// responsible for refusing when credentials are missing.
func DialWS(url string, headers http.Header) Dialer {
	return func(ctx context.Context) (Conn, error) {
		c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			return nil, err
		}
		// Order books can be large; lift the default read limit.
		c.SetReadLimit(8 << 20)
		return wsConn{c}, nil
	}
}
