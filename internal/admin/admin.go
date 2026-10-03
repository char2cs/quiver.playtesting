// Package admin exposes a core.Service over a local unix socket.
//
// Protocol: line-delimited JSON, lines at most 64KiB. A request is
// {"id":1,"method":"AddVM","params":{...}}. The reply is
// {"id":1,"result":...} or {"id":1,"error":"msg","code":"not_found|busy"}.
// A connection serves requests one after another with a 30s read deadline.
// Method "Subscribe" replies with an empty result, then the server streams
// {"event":{...}} lines until either side closes the connection.
// The socket is mode 0600 and the server rejects peers with a different uid.
// VM passwords travel only in AddVM params and are never sent back.
package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"quiver-playtesting/internal/core"
)

const (
	maxLine     = 64 << 10
	connTimeout = 30 * time.Second
)

type request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
	Event  *core.Event     `json:"event,omitempty"`
}

type addVMParams struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

type idParams struct {
	ID int64 `json:"id"`
}

type createLinkParams struct {
	VMID  int64         `json:"vm_id"`
	Label string        `json:"label"`
	TTL   time.Duration `json:"ttl"`
}

type createLinkResult struct {
	Link  core.Link `json:"link"`
	Token string    `json:"token"`
}

type killParams struct {
	SessionID  string `json:"session_id"`
	RevokeLink bool   `json:"revoke_link"`
}

type sessionParams struct {
	SessionID string `json:"session_id"`
}

func newScanner(c net.Conn) *bufio.Scanner {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	return sc
}

func writeJSON(c net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.SetWriteDeadline(time.Now().Add(connTimeout))
	_, err = c.Write(append(b, '\n'))
	return err
}

func codeOf(err error) string {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return "not_found"
	case errors.Is(err, core.ErrBusy):
		return "busy"
	}
	return ""
}

// PrepareDir creates dir with mode 0700 and tightens it if it already exists
// with wider permissions. It refuses symlinks and directories owned by someone else.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by another user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// Serve listens on socketPath until ctx is done. The socket's directory is
// forced to 0700 first, so the window between bind and chmod is not reachable by other users.
func Serve(ctx context.Context, socketPath string, svc core.Service) error {
	if err := PrepareDir(filepath.Dir(socketPath)); err != nil {
		return err
	}
	if err := clearStale(socketPath); err != nil {
		return err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer os.Remove(socketPath)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer func() {
				if v := recover(); v != nil {
					slog.Error("admin connection panic", "panic", v)
				}
			}()
			serveConn(ctx, c, svc)
		}()
	}
}

func clearStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("another daemon is already listening on %s", path)
	}
	return os.Remove(path)
}

func serveConn(ctx context.Context, c net.Conn, svc core.Service) {
	defer c.Close()
	if !peerAllowed(c) {
		return
	}
	sc := newScanner(c)
	for {
		c.SetReadDeadline(time.Now().Add(connTimeout))
		if !sc.Scan() {
			if errors.Is(sc.Err(), bufio.ErrTooLong) {
				writeJSON(c, response{Error: "request too large"})
			}
			return
		}
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			writeJSON(c, response{Error: "bad request"})
			return
		}
		if req.Method == "Subscribe" {
			stream(ctx, c, sc, svc, req.ID)
			return
		}
		res, err := dispatch(ctx, svc, req)
		resp := response{ID: req.ID}
		if err != nil {
			resp.Error, resp.Code = err.Error(), codeOf(err)
		} else if resp.Result, err = json.Marshal(res); err != nil {
			resp.Error = "internal error"
		}
		if writeJSON(c, resp) != nil {
			return
		}
	}
}

func stream(ctx context.Context, c net.Conn, sc *bufio.Scanner, svc core.Service, id int64) {
	events, unsub := svc.Subscribe()
	defer unsub()
	if writeJSON(c, response{ID: id}) != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	gone := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(gone)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gone:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if writeJSON(c, response{Event: &ev}) != nil {
				return
			}
		}
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}

func dispatch(ctx context.Context, svc core.Service, req request) (any, error) {
	switch req.Method {
	case "ListVMs":
		vms, err := svc.ListVMs(ctx)
		return nonNil(vms), err
	case "AddVM":
		p, err := decode[addVMParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return svc.AddVM(ctx, p.Name, p.Host, p.Port, p.Password)
	case "RemoveVM":
		p, err := decode[idParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return nil, svc.RemoveVM(ctx, p.ID)
	case "TestVM":
		p, err := decode[idParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return nil, svc.TestVM(ctx, p.ID)
	case "ListLinks":
		ls, err := svc.ListLinks(ctx)
		return nonNil(ls), err
	case "CreateLink":
		p, err := decode[createLinkParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		l, tok, err := svc.CreateLink(ctx, p.VMID, p.Label, p.TTL)
		return createLinkResult{l, tok}, err
	case "RevokeLink":
		p, err := decode[idParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return nil, svc.RevokeLink(ctx, p.ID)
	case "ListSessions":
		return nonNil(svc.ListSessions()), nil
	case "Kill":
		p, err := decode[killParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return nil, svc.Kill(p.SessionID, p.RevokeLink)
	case "Flag":
		p, err := decode[sessionParams](req.Params)
		if err != nil {
			return nil, errors.New("bad params")
		}
		return nil, svc.Flag(p.SessionID)
	}
	return nil, errors.New("unknown method")
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
