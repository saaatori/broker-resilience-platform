// sse.go 进程内 SSE hub（M2 第 34 天）：按 task_id 广播执行事件与任务状态变化。
// 单副本部署（多 Agent 横向扩展已 CUT）——进程内 map 即全量路由，无跨实例广播需求。
// 事件不持久化：断线不回放（协议 §2.4 v1.0.5），前端以任务状态查询兜底（M4 注意）。
package task

import (
	"log"
	"sync"
)

// SSEEvent 一条 SSE 消息：Name=事件名（status/log/progress/task_status），Data 序列化为 data: 行。
type SSEEvent struct {
	Name string
	Data any
}

// hubSubBuffer 单订阅者缓冲：超出即丢弃（慢消费者不拖垮发布路径——Agent 上报不能被前端卡顿反压）。
const hubSubBuffer = 16

// Hub task_id → 订阅者集合。
type Hub struct {
	mu   sync.RWMutex
	subs map[int64]map[chan SSEEvent]struct{}
}

// NewHub 构造空 hub（main 单例装配）。
func NewHub() *Hub {
	return &Hub{subs: map[int64]map[chan SSEEvent]struct{}{}}
}

// Subscribe 订阅某任务的事件流；返回只读通道与取消函数（重复调用安全）。
func (h *Hub) Subscribe(taskID int64) (<-chan SSEEvent, func()) {
	ch := make(chan SSEEvent, hubSubBuffer)
	h.mu.Lock()
	if h.subs[taskID] == nil {
		h.subs[taskID] = map[chan SSEEvent]struct{}{}
	}
	h.subs[taskID][ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs[taskID], ch)
			if len(h.subs[taskID]) == 0 {
				delete(h.subs, taskID)
			}
			h.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Publish 向任务全部订阅者非阻塞投递；缓冲满则丢弃该订阅者本条并记日志（保 Agent 上报路径不被反压）。
func (h *Hub) Publish(taskID int64, ev SSEEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[taskID] {
		select {
		case ch <- ev:
		default:
			log.Printf("task: SSE 订阅者缓冲满，丢弃事件（task=%d name=%s）", taskID, ev.Name)
		}
	}
}
