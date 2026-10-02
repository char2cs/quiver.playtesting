package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"quiver-playtesting/internal/core"
)

// Client implements core.Service over the admin socket.
type Client struct {
	path   string
	nextID atomic.Int64
}

var _ core.Service = (*Client)(nil)

// Dial checks that a daemon is reachable at socketPath.
func Dial(socketPath string) (*Client, error) {
	c, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return nil, err
	}
	c.Close()
	return &Client{path: socketPath}, nil
}

// Close is a no-op, connections are per call.
func (c *Client) Close() error { return nil }

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.path)
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	req := request{ID: c.nextID.Add(1), Method: method}
	if params != nil {
		if req.Params, err = json.Marshal(params); err != nil {
			return err
		}
	}
	if err := writeJSON(conn, req); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Now().Add(connTimeout))
	resp, err := readResponse(newScanner(conn))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if out != nil && len(resp.Result) > 0 {
		return json.Unmarshal(resp.Result, out)
	}
	return nil
}

func readResponse(sc *bufio.Scanner) (response, error) {
	var resp response
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return resp, err
		}
		return resp, errors.New("connection closed")
	}
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return resp, err
	}
	switch resp.Code {
	case "not_found":
		return resp, core.ErrNotFound
	case "busy":
		return resp, core.ErrBusy
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func (c *Client) ListVMs(ctx context.Context) ([]core.VM, error) {
	var out []core.VM
	err := c.call(ctx, "ListVMs", nil, &out)
	return out, err
}

func (c *Client) AddVM(ctx context.Context, name, host string, port int, password string) (core.VM, error) {
	var out core.VM
	err := c.call(ctx, "AddVM", addVMParams{name, host, port, password}, &out)
	return out, err
}

func (c *Client) RemoveVM(ctx context.Context, id int64) error {
	return c.call(ctx, "RemoveVM", idParams{id}, nil)
}

func (c *Client) TestVM(ctx context.Context, id int64) error {
	return c.call(ctx, "TestVM", idParams{id}, nil)
}

func (c *Client) ListLinks(ctx context.Context) ([]core.Link, error) {
	var out []core.Link
	err := c.call(ctx, "ListLinks", nil, &out)
	return out, err
}

func (c *Client) CreateLink(ctx context.Context, vmID int64, label string, ttl time.Duration) (core.Link, string, error) {
	var out createLinkResult
	err := c.call(ctx, "CreateLink", createLinkParams{vmID, label, ttl}, &out)
	return out.Link, out.Token, err
}

func (c *Client) RevokeLink(ctx context.Context, id int64) error {
	return c.call(ctx, "RevokeLink", idParams{id}, nil)
}

// ListSessions returns nil if the daemon is unreachable.
func (c *Client) ListSessions() []core.Session {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []core.Session
	if c.call(ctx, "ListSessions", nil, &out) != nil {
		return nil
	}
	return out
}

func (c *Client) Kill(sessionID string, revokeLink bool) error {
	return c.call(context.Background(), "Kill", killParams{sessionID, revokeLink}, nil)
}

func (c *Client) Flag(sessionID string) error {
	return c.call(context.Background(), "Flag", sessionParams{sessionID}, nil)
}

// Subscribe streams events on its own connection. The channel closes when the
// stream ends or the returned func is called.
func (c *Client) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event, 64)
	conn, err := c.dial(context.Background())
	if err != nil {
		close(ch)
		return ch, func() {}
	}
	unsub := func() { conn.Close() }
	if writeJSON(conn, request{ID: c.nextID.Add(1), Method: "Subscribe"}) != nil {
		conn.Close()
		close(ch)
		return ch, unsub
	}
	sc := newScanner(conn)
	conn.SetReadDeadline(time.Now().Add(connTimeout))
	if _, err := readResponse(sc); err != nil {
		conn.Close()
		close(ch)
		return ch, unsub
	}
	conn.SetReadDeadline(time.Time{})
	go func() {
		defer close(ch)
		defer conn.Close()
		for sc.Scan() {
			var r response
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.Event == nil {
				continue
			}
			ch <- *r.Event
		}
	}()
	return ch, unsub
}
