package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
)

// maxFrameBytes bounds one JSON-RPC frame read from an agent. ACP frames are
// newline-delimited JSON; a tool result can be large, a runaway one is not.
const maxFrameBytes = 64 << 20

// JSON-RPC 2.0 error codes used by the adapter (the ACP ErrorCode set).
const (
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
	rpcAuthRequired   = -32000
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return fmt.Sprintf("%s (code %d): %s", e.Message, e.Code, truncate(string(e.Data), 300))
	}
	return fmt.Sprintf("%s (code %d)", e.Message, e.Code)
}

type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

// rpcHandler receives what the agent sends the client. Notify runs on the
// reader goroutine, in arrival order, and must not block: every update the
// agent sends before a response is therefore handled before that response is
// delivered. Request runs on its own goroutine (a request such as
// terminal/wait_for_exit blocks); ctx ends when the connection closes.
type rpcHandler interface {
	Notify(method string, params json.RawMessage)
	Request(ctx context.Context, method string, params json.RawMessage) (any, *rpcError)
}

// rpcConn is one JSON-RPC 2.0 connection over a newline-delimited stream
// (an ACP agent's stdin/stdout).
type rpcConn struct {
	w       io.Writer
	wmu     sync.Mutex
	handler rpcHandler
	label   string

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcReply
	err     error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

var errConnClosed = errors.New("acp connection closed")

func newRPCConn(r io.Reader, w io.Writer, h rpcHandler, label string) *rpcConn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &rpcConn{w: w, handler: h, label: label, pending: map[int64]chan rpcReply{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go c.read(r)
	return c
}

// Done is closed once the stream ended; Err tells why.
func (c *rpcConn) Done() <-chan struct{} { return c.done }

func (c *rpcConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *rpcConn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	var err error
	for {
		var line []byte
		line, err = readFrame(br)
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(bytes.TrimSpace(line))
		}
		if err != nil {
			break
		}
	}
	if errors.Is(err, io.EOF) {
		err = errConnClosed
	}
	c.mu.Lock()
	c.err = err
	pending := c.pending
	c.pending = map[int64]chan rpcReply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- rpcReply{err: err}
	}
	c.cancel()
	close(c.done)
}

// readFrame reads one newline-terminated frame, refusing a frame larger than
// maxFrameBytes (the stream is then unusable).
func readFrame(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxFrameBytes {
			return nil, fmt.Errorf("acp frame exceeds %d bytes", maxFrameBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, err
	}
}

func (c *rpcConn) dispatch(line []byte) {
	if line[0] != '{' {
		// An agent's dependency printing to stdout is noise, not protocol.
		slog.Debug("bnbloc-acp skipping non-JSON agent output", "agent", c.label, "line", truncate(string(line), 200))
		return
	}
	var f rpcFrame
	if err := json.Unmarshal(line, &f); err != nil {
		slog.Warn("bnbloc-acp malformed agent frame", "agent", c.label, "error", err)
		return
	}
	hasID := len(f.ID) > 0 && string(f.ID) != "null"
	switch {
	case f.Method != "" && hasID:
		go c.serve(f)
	case f.Method != "":
		c.handler.Notify(f.Method, f.Params)
	case hasID:
		id, err := strconv.ParseInt(string(f.ID), 10, 64)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch == nil {
			return
		}
		if f.Error != nil {
			ch <- rpcReply{err: f.Error}
			return
		}
		ch <- rpcReply{result: f.Result}
	}
}

func (c *rpcConn) serve(f rpcFrame) {
	result, rerr := c.handler.Request(c.ctx, f.Method, f.Params)
	out := rpcFrame{JSONRPC: "2.0", ID: f.ID}
	if rerr != nil {
		out.Error = rerr
	} else {
		if result == nil {
			result = struct{}{}
		}
		raw, err := json.Marshal(result)
		if err != nil {
			out.Error = &rpcError{Code: rpcInternalError, Message: err.Error()}
		} else {
			out.Result = raw
		}
	}
	_ = c.write(out)
}

func (c *rpcConn) write(f rpcFrame) error {
	f.JSONRPC = "2.0"
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(raw)
	return err
}

// Call sends a request and waits for its response (decoded into out when
// non-nil), the context, or the end of the connection.
func (c *rpcConn) Call(ctx context.Context, method string, params, out any) error {
	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(rpcFrame{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: rawParams}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 && string(r.result) != "null" {
			if err := json.Unmarshal(r.result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends a notification (no response).
func (c *rpcConn) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.write(rpcFrame{Method: method, Params: raw})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
