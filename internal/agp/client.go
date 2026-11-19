package agp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed 表示客户端连接已关闭。
var ErrClosed = errors.New("agp: client closed")

// Client 是 AGP 协议的通用客户端：负责 JSONL 帧的编解码、按请求 ID 的多路复用，
// 以及运行中断（abort）的投递。它不关心对端是 gp 的 RPC 服务端还是别的实现。
//
// 同一个 Client 可承载多条并发运行；每轮运行通过 Run 返回的事件通道独立消费。
type Client struct {
	conn      net.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once

	mu      sync.Mutex
	streams map[string]*stream    // 长事件流（run）
	waiters map[string]chan Event // 单次应答（hello/ping/abort/reset）
	closed  bool
	err     error
}

// stream 是一条运行的事件通道。push/close 由内部互斥保护，避免向已关闭通道写入。
type stream struct {
	mu     sync.Mutex
	ch     chan Event
	done   chan struct{}
	closed bool
}

// push 把事件投递到消费方。缓冲区满时阻塞，形成对服务端的自然背压。
// 调用方必须持续消费通道直至其关闭，否则会阻塞读取循环。
func (s *stream) push(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.ch <- ev
}

// close 关闭事件通道；可安全重复调用。
func (s *stream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.ch)
	close(s.done)
}

var requestSeq atomic.Uint64

// NewRequest 生成一个带唯一 ID 的请求帧。
func NewRequest(op string) Request {
	return Request{
		ID: fmt.Sprintf("r%d", requestSeq.Add(1)),
		Op: op,
	}
}

// Dial 按 endpoint（unix:// 或 tcp://）连接 AGP 服务端。
func Dial(ctx context.Context, endpoint string) (*Client, error) {
	network, addr, err := ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s %s: %w", network, addr, err)
	}
	return NewClient(conn), nil
}

// NewClient 基于已有连接构造客户端（便于测试注入 net.Pipe 等传输）。
func NewClient(conn net.Conn) *Client {
	c := &Client{
		conn:    conn,
		streams: make(map[string]*stream),
		waiters: make(map[string]chan Event),
	}
	go c.readLoop()
	return c
}

// ParseEndpoint 解析 unix:// 或 tcp:// 形式的目标地址。
func ParseEndpoint(endpoint string) (string, string, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return "unix", strings.TrimPrefix(endpoint, "unix://"), nil
	case strings.HasPrefix(endpoint, "tcp://"):
		return "tcp", strings.TrimPrefix(endpoint, "tcp://"), nil
	default:
		return "", "", fmt.Errorf("unsupported endpoint %q (expected unix://... or tcp://...)", endpoint)
	}
}

// Close 关闭底层连接，并让所有未完成的运行返回。
func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.fail(ErrClosed)
		err = c.conn.Close()
	})
	return err
}

// fail 让所有等待方感知连接中断。
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	streams := c.streams
	waiters := c.waiters
	c.streams = make(map[string]*stream)
	c.waiters = make(map[string]chan Event)
	c.mu.Unlock()

	for _, s := range streams {
		s.close()
	}
	for _, ch := range waiters {
		close(ch)
	}
}

func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		c.route(ev)
	}
	err := scanner.Err()
	if err == nil {
		err = ErrClosed
	}
	c.fail(err)
}

func (c *Client) route(ev Event) {
	c.mu.Lock()
	s := c.streams[ev.ID]
	w := c.waiters[ev.ID]
	if w != nil {
		delete(c.waiters, ev.ID)
	}
	if s != nil && ev.Terminal() {
		delete(c.streams, ev.ID)
	}
	c.mu.Unlock()

	if s != nil {
		s.push(ev)
		if ev.Terminal() {
			s.close()
		}
		return
	}
	if w != nil {
		w <- ev
	}
}

func (c *Client) send(req Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if _, err := c.conn.Write(data); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	return nil
}

// request 发送请求并等待一条同 ID 的应答事件。
func (c *Client) request(ctx context.Context, req Request) (Event, error) {
	ch := make(chan Event, 1)

	c.mu.Lock()
	if c.closed {
		closedErr := c.err
		c.mu.Unlock()
		if closedErr == nil {
			closedErr = ErrClosed
		}
		return Event{}, closedErr
	}
	c.waiters[req.ID] = ch
	c.mu.Unlock()

	if err := c.send(req); err != nil {
		c.mu.Lock()
		delete(c.waiters, req.ID)
		c.mu.Unlock()
		return Event{}, err
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			c.mu.Lock()
			closedErr := c.err
			c.mu.Unlock()
			if closedErr == nil {
				closedErr = ErrClosed
			}
			return Event{}, closedErr
		}
		return ev, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, req.ID)
		c.mu.Unlock()
		return Event{}, ctx.Err()
	}
}

// Hello 执行握手，返回 hello_ack（含 agent 名称与版本）。
func (c *Client) Hello(ctx context.Context, clientName string) (Event, error) {
	req := NewRequest(OpHello)
	req.Client = clientName
	ev, err := c.request(ctx, req)
	if err != nil {
		return Event{}, err
	}
	if ev.Event == EventError {
		return ev, fmt.Errorf("hello rejected: %s", ev.Message)
	}
	return ev, nil
}

// Ping 探测服务端存活。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.request(ctx, NewRequest(OpPing))
	return err
}

// Abort 中断指定 ID 的运行（targetID 为 run 请求的 ID）。
func (c *Client) Abort(ctx context.Context, targetID string) error {
	req := NewRequest(OpAbort)
	req.TargetID = targetID
	_, err := c.request(ctx, req)
	return err
}

// ResetSession 重置指定会话，使后续运行落到新的会话代次上。
func (c *Client) ResetSession(ctx context.Context, sessionID string) error {
	req := NewRequest(OpResetSession)
	req.SessionID = sessionID
	_, err := c.request(ctx, req)
	return err
}

// Run 发起一次运行并返回事件通道。
//
// 通道在收到 final / error 事件、ctx 取消或连接断开时关闭。ctx 取消时客户端会
// 尽力发送 abort 让服务端停止推理。调用方需持续消费通道直至关闭。
func (c *Client) Run(ctx context.Context, req Request) (<-chan Event, error) {
	if req.ID == "" {
		req.ID = NewRequest(req.Op).ID
	}

	s := &stream{
		ch:   make(chan Event, 64),
		done: make(chan struct{}),
	}

	c.mu.Lock()
	if c.closed {
		closedErr := c.err
		c.mu.Unlock()
		if closedErr == nil {
			closedErr = ErrClosed
		}
		return nil, closedErr
	}
	c.streams[req.ID] = s
	c.mu.Unlock()

	if err := c.send(req); err != nil {
		c.mu.Lock()
		delete(c.streams, req.ID)
		c.mu.Unlock()
		s.close()
		return nil, err
	}

	go c.watchCancel(ctx, req.ID, s)
	return s.ch, nil
}

// watchCancel 在 ctx 取消时投递 abort 并收尾事件流；运行正常结束时随 done 退出。
func (c *Client) watchCancel(ctx context.Context, id string, s *stream) {
	select {
	case <-ctx.Done():
	case <-s.done:
		return
	}

	abortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = c.Abort(abortCtx, id)
	cancel()

	c.mu.Lock()
	delete(c.streams, id)
	c.mu.Unlock()
	s.close()
}
