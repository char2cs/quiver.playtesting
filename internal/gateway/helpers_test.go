package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func dialWS(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws/"+token, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": {"http://" + addr}},
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func dialMany(t *testing.T, addr string, n int) []net.Conn {
	t.Helper()
	var out []net.Conn
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	time.Sleep(100 * time.Millisecond)
	return out
}

func closeAll(cs []net.Conn) {
	for _, c := range cs {
		c.Close()
	}
}

func isTimeout(err error) bool { return errors.Is(err, os.ErrDeadlineExceeded) }
