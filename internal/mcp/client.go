package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Client MCP stdio 传输客户端
type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.Reader
	closer    io.Closer
	mu        sync.Mutex
	writeMu   sync.Mutex
	reqID     atomic.Uint64
	pending   map[uint64]chan *Response
	done      chan struct{}
	closeOnce sync.Once
	err       error
}

// NewStdioClient 启动子进程并建立 stdio JSON-RPC 通信客户端
func NewStdioClient(ctx context.Context, command string, args []string, env map[string]string) (*Client, error) {
	cmd := exec.CommandContext(ctx, command, args...)

	// 注入环境变量
	if len(env) > 0 {
		mergedEnv := os.Environ()
		for k, v := range env {
			mergedEnv = append(mergedEnv, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = mergedEnv
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start mcp process (%s): %w", command, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		pending: make(map[uint64]chan *Response),
		done:    make(chan struct{}),
	}

	go c.readLoop()
	return c, nil
}

// NewClientFromReadWriter 根据传入的 Reader/WriteCloser 构建客户端 (主要用于 Mock 单元测试)
func NewClientFromReadWriter(r io.Reader, w io.WriteCloser, closer io.Closer) *Client {
	c := &Client{
		stdin:   w,
		stdout:  r,
		closer:  closer,
		pending: make(map[uint64]chan *Response),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// readLoop 持续监听 stdout 流并分发 JSON-RPC 响应
func (c *Client) readLoop() {
	reader := bufio.NewReader(c.stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var resp Response
				if jsonErr := json.Unmarshal(trimmed, &resp); jsonErr == nil && resp.ID != nil {
					c.mu.Lock()
					ch, ok := c.pending[*resp.ID]
					if ok {
						delete(c.pending, *resp.ID)
					}
					c.mu.Unlock()

					if ok && ch != nil {
						ch <- &resp
					}
				}
			}
		}

		if err != nil {
			c.closeWithError(err)
			return
		}
	}
}

func (c *Client) closeWithError(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = err
		close(c.done)
		// 唤醒所有挂起的等待协程
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()

		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.closer != nil {
			_ = c.closer.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			// 给子进程缓冲时间正常退出，超时则直接 Kill
			doneCh := make(chan error, 1)
			go func() {
				doneCh <- c.cmd.Wait()
			}()

			select {
			case <-doneCh:
			case <-time.After(1 * time.Second):
				_ = c.cmd.Process.Kill()
			}
		}
	})
}

// Call 发起双向阻塞式 RPC 调用
func (c *Client) Call(ctx context.Context, method string, params any) (*Response, error) {
	select {
	case <-c.done:
		return nil, fmt.Errorf("client is closed: %w", c.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	id := c.reqID.Add(1)

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	req := Request{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  rawParams,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal jsonrpc request: %w", err)
	}
	data = append(data, '\n')

	respChan := make(chan *Response, 1)
	c.mu.Lock()
	c.pending[id] = respChan
	c.mu.Unlock()

	c.writeMu.Lock()
	_, writeErr := c.stdin.Write(data)
	c.writeMu.Unlock()

	if writeErr != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write request: %w", writeErr)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("client closed: %w", c.err)
	case resp, ok := <-respChan:
		if !ok || resp == nil {
			return nil, errors.New("connection closed before response received")
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	}
}

// Notify 发送单向通知 (不期待应答)
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	select {
	case <-c.done:
		return fmt.Errorf("client is closed: %w", c.err)
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal notification params: %w", err)
		}
		rawParams = b
	}

	req := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal jsonrpc notification: %w", err)
	}
	data = append(data, '\n')

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, writeErr := c.stdin.Write(data)
	return writeErr
}

// Initialize 完成 MCP 协议标准握手 (initialize -> notifications/initialized)
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	initParams := InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities:    map[string]any{},
		ClientInfo: ClientInfo{
			Name:    "gp",
			Version: "v0.1.0-alpha",
		},
	}

	resp, err := c.Call(ctx, "initialize", initParams)
	if err != nil {
		return nil, fmt.Errorf("initialize rpc: %w", err)
	}

	var initRes InitializeResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &initRes); err != nil {
			return nil, fmt.Errorf("unmarshal initialize result: %w", err)
		}
	}

	// 发送 notifications/initialized 通知确认握手完成
	_ = c.Notify(ctx, "notifications/initialized", map[string]any{})

	return &initRes, nil
}

// ListTools 查询远端 MCP 暴露的所有工具列表
func (c *Client) ListTools(ctx context.Context) ([]ToolDefinition, error) {
	resp, err := c.Call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("tools/list rpc: %w", err)
	}

	var listRes ListToolsResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &listRes); err != nil {
			return nil, fmt.Errorf("unmarshal tools/list result: %w", err)
		}
	}
	return listRes.Tools, nil
}

// CallTool 执行远端 MCP 暴露的工具调用
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	params := CallToolParams{
		Name:      name,
		Arguments: args,
	}

	resp, err := c.Call(ctx, "tools/call", params)
	if err != nil {
		return nil, fmt.Errorf("tools/call (%s) rpc: %w", name, err)
	}

	var callRes CallToolResult
	if len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, &callRes); err != nil {
			return nil, fmt.Errorf("unmarshal tools/call result: %w", err)
		}
	}
	return &callRes, nil
}

// Close 关闭客户端与底层进程
func (c *Client) Close() error {
	c.closeWithError(io.EOF)
	return nil
}
