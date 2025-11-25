package notify

import (
	"log"
	"sync"
)

// -------------------------
// 通知发送接口
// -------------------------
type Notifier interface {
	Send(message UnifiedMessage) error
}

// -------------------------
// 消息队列管理
// -------------------------
var (
	messageChan  chan UnifiedMessage
	notifiers    []Notifier
	once         sync.Once
	shutdownChan chan struct{}
)

// InitNotify 初始化消息模块
func InitNotify() {
	once.Do(func() {
		messageChan = make(chan UnifiedMessage, 100) // buffer 可调
		shutdownChan = make(chan struct{})
		notifiers = []Notifier{
			&EmailNotifier{},
			&WebhookNotifier{},
		}
		go messageLoop()
	})
}

// StopNotify 停止消息监听
func StopNotify() {
	close(shutdownChan)
}

// SendMessage 推送消息到队列
func SendMessage(msg UnifiedMessage) {
	if messageChan != nil {
		messageChan <- msg
	}
}

// -------------------------
// 消息处理循环
// -------------------------
func messageLoop() {
	for {
		select {
		case <-shutdownChan:
			log.Println("[Notify] Message loop stopped")
			return
		case msg := <-messageChan:
			dispatch(msg)
		}
	}
}

// dispatch 将消息发送给所有通知通道
func dispatch(msg UnifiedMessage) {
	for _, n := range notifiers {
		if err := n.Send(msg); err != nil {
			log.Printf("[Notify] Failed to send via %T: %v", n, err)
		}
	}
}
