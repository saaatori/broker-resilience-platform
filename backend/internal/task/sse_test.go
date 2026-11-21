package task

import (
	"testing"
	"time"
)

// TestHubDeliversToAllSubscribers 单/多订阅者广播；无订阅者 Publish 不阻塞。
func TestHubDeliversToAllSubscribers(t *testing.T) {
	h := NewHub()
	ch1, cancel1 := h.Subscribe(42)
	ch2, _ := h.Subscribe(42)
	defer cancel1()

	h.Publish(42, SSEEvent{Name: "log", Data: "hello"})
	select {
	case ev := <-ch1:
		if ev.Data != "hello" {
			t.Errorf("ch1 数据不符: %v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 未收到")
	}
	select {
	case <-ch2:
	default:
		t.Fatal("ch2 未收到")
	}

	h.Publish(43, SSEEvent{Name: "log", Data: "other-task"}) // 其他任务不串扰
	select {
	case ev := <-ch1:
		t.Fatalf("不应收到其他任务的事件: %v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHubCancelStopsDelivery 取消后不再投递（订阅者清理）。
// 注：cancel 会 close(ch)——closed channel 接收恒立即可行（ok=false 零值），非投递。
func TestHubCancelStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, cancel := h.Subscribe(7)
	cancel()
	h.Publish(7, SSEEvent{Name: "x", Data: 1})
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("取消后不应投递，收到 %+v", ev)
		} // ok=false=close 正常表现
	case <-time.After(50 * time.Millisecond):
		// 未 close 也未投递——同样合格
	}
}

// TestHubNoSubscriberNoBlock 无订阅者/慢消费者（缓冲满）Publish 不阻塞（丢弃策略）。
func TestHubNoSubscriberNoBlock(t *testing.T) {
	h := NewHub()
	done := make(chan struct{})
	go func() {
		h.Publish(1, SSEEvent{Name: "x", Data: 1}) // 无订阅者
		ch, _ := h.Subscribe(2)
		for i := 0; i < 100; i++ { // 远超缓冲——慢消费者
			h.Publish(2, SSEEvent{Name: "x", Data: i})
		}
		_ = ch
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 被阻塞（慢消费者拖垮发布路径）")
	}
}

// TestHubNoSubscriberDoesNotPanicAndCancelTwice 重复 cancel 防御。
func TestHubCancelTwice(t *testing.T) {
	h := NewHub()
	_, cancel := h.Subscribe(9)
	cancel()
	cancel() // 不 panic
}
