// Package httpserver HTTP 服务器装配——超时口径的唯一落点。
// 此前用 gin 的 r.Run（net/http 零超时）：慢速/半开连接可无限占用 goroutine 与文件描述符。
// 全局超时对普通请求足够；长连接流式响应（SSE）由处理器显式调用 StreamNoWriteDeadline 解除写超时。
package httpserver

import (
	"errors"
	"net/http"
	"time"
)

// Timeouts 服务超时口径（net/http 文档建议：务必设置 ReadHeaderTimeout；Read/Write 按业务上界取值）。
type Timeouts struct {
	ReadHeader time.Duration // 请求头读取上限——防 slowloris（慢速发头占连接）
	Read       time.Duration // 请求体读取上限：4MB 结果体 30s 内传完≈需 140KB/s；更慢的链路会在 30s 被切断
	Write      time.Duration // 响应写上限；SSE 等流式响应逐连接解除（见 StreamNoWriteDeadline）
	Idle       time.Duration // keep-alive 空闲连接回收
}

// Default 默认口径。
var Default = Timeouts{
	ReadHeader: 10 * time.Second,
	Read:       30 * time.Second,
	Write:      60 * time.Second,
	Idle:       120 * time.Second,
}

// New 以默认超时构造服务器。
func New(addr string, handler http.Handler) *http.Server {
	return NewWithTimeouts(addr, handler, Default)
}

// NewWithTimeouts 以指定超时构造服务器（测试可注入短超时验证行为）。
func NewWithTimeouts(addr string, handler http.Handler, t Timeouts) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: t.ReadHeader,
		ReadTimeout:       t.Read,
		WriteTimeout:      t.Write,
		IdleTimeout:       t.Idle,
	}
}

// ListenAndServe 以默认超时阻塞服务；返回错误即服务无法继续（main 直接 Fatal）。
func ListenAndServe(addr string, handler http.Handler) error {
	srv := New(addr, handler)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// StreamNoWriteDeadline 解除当前连接的写超时——长连接流式响应（SSE）必须调用，
// 否则流在全局 WriteTimeout 到点后被掐断。经 http.ResponseController 逐层 Unwrap 到底层连接
// （gin 的 responseWriter 实现了 Unwrap）。连接关闭即失效，无需恢复。
func StreamNoWriteDeadline(w http.ResponseWriter) error {
	return http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
