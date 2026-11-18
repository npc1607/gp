package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrPeerNotFound 目标 Agent 信箱未注册
	ErrPeerNotFound = errors.New("peer mailbox not found")
	// ErrMailboxFull 目标 Agent 信箱已满
	ErrMailboxFull = errors.New("peer mailbox is full")
)

// PeerReply 封装点对点 A2A 消息的同步应答结果
type PeerReply struct {
	From    string
	Payload string
	Err     error
}

// PeerMessage 描述点对点 A2A 信箱中的一条消息
type PeerMessage struct {
	ID        uint64
	From      string
	To        string
	Payload   string
	CreatedAt time.Time
	replyCh   chan PeerReply
	replied   atomic.Bool
}

// Reply 向发送方回传应答（仅首次调用生效，并发安全）
func (m *PeerMessage) Reply(payload string, err error) {
	if m.replyCh == nil {
		return
	}
	if m.replied.CompareAndSwap(false, true) {
		m.replyCh <- PeerReply{
			From:    m.To,
			Payload: payload,
			Err:     err,
		}
	}
}

// PeerHandler 基于回调的点对点消息处理器
type PeerHandler func(ctx context.Context, msg *PeerMessage) (string, error)

type peerEndpoint struct {
	inbox   chan *PeerMessage
	handler PeerHandler
}

// MailboxRouter 拓扑 B：点对点 A2A 信箱路由器 (Actor Mailbox)。
// 支持基于 Channel 的协程信箱以及回调式处理器，提供同步请求-应答 (Send) 与异步投递 (Post)。
type MailboxRouter struct {
	mu    sync.RWMutex
	seq   atomic.Uint64
	peers map[string]*peerEndpoint
}

// NewMailboxRouter 创建点对点信箱路由器
func NewMailboxRouter() *MailboxRouter {
	return &MailboxRouter{
		peers: make(map[string]*peerEndpoint),
	}
}

// RegisterMailbox 为指定 peerID 注册 Channel 驱动的 Actor 信箱，返回只读收件箱与注销函数
func (r *MailboxRouter) RegisterMailbox(peerID string, bufSize int) (<-chan *PeerMessage, func()) {
	key := normalizePeerID(peerID)
	if bufSize <= 0 {
		bufSize = 16
	}
	ch := make(chan *PeerMessage, bufSize)
	ep := &peerEndpoint{inbox: ch}

	r.mu.Lock()
	r.peers[key] = ep
	r.mu.Unlock()

	var once sync.Once
	unregister := func() {
		once.Do(func() {
			r.mu.Lock()
			if cur, ok := r.peers[key]; ok && cur == ep {
				delete(r.peers, key)
			}
			r.mu.Unlock()
		})
	}
	return ch, unregister
}

// RegisterHandler 为指定 peerID 注册同步/异步回调处理器，返回注销函数
func (r *MailboxRouter) RegisterHandler(peerID string, handler PeerHandler) func() {
	key := normalizePeerID(peerID)
	ep := &peerEndpoint{handler: handler}

	r.mu.Lock()
	r.peers[key] = ep
	r.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if cur, ok := r.peers[key]; ok && cur == ep {
				delete(r.peers, key)
			}
			r.mu.Unlock()
		})
	}
}

// HasPeer 判断目标 peerID 是否已注册信箱或处理器
func (r *MailboxRouter) HasPeer(peerID string) bool {
	key := normalizePeerID(peerID)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.peers[key]
	return ok
}

// ListPeers 返回当前已注册的所有 peerID（已排序）
func (r *MailboxRouter) ListPeers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.peers))
	for k := range r.peers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Send 向目标 peer 发送点对点消息并阻塞等待应答或 ctx 超时/取消
func (r *MailboxRouter) Send(ctx context.Context, from, to, payload string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	key := normalizePeerID(to)
	r.mu.RLock()
	ep, ok := r.peers[key]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrPeerNotFound, to)
	}

	msg := &PeerMessage{
		ID:        r.seq.Add(1),
		From:      from,
		To:        key,
		Payload:   payload,
		CreatedAt: time.Now(),
		replyCh:   make(chan PeerReply, 1),
	}

	if ep.handler != nil {
		return ep.handler(ctx, msg)
	}

	select {
	case ep.inbox <- msg:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	select {
	case reply := <-msg.replyCh:
		return reply.Payload, reply.Err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Post 向目标 peer 投递无需等待应答的异步点对点消息
func (r *MailboxRouter) Post(ctx context.Context, from, to, payload string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	key := normalizePeerID(to)
	r.mu.RLock()
	ep, ok := r.peers[key]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrPeerNotFound, to)
	}

	msg := &PeerMessage{
		ID:        r.seq.Add(1),
		From:      from,
		To:        key,
		Payload:   payload,
		CreatedAt: time.Now(),
	}

	if ep.handler != nil {
		go func() {
			_, _ = ep.handler(ctx, msg)
		}()
		return nil
	}

	select {
	case ep.inbox <- msg:
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrMailboxFull, to)
	}
}

func normalizePeerID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
