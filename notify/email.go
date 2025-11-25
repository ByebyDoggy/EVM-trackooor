package notify

import (
	"fmt"

	"github.com/Zellic/EVM-trackooor/shared"
)

// -------------------------
// Email 通道示例
// -------------------------
type EmailNotifier struct {
	// 可以加 SMTP 配置字段
}

func (e *EmailNotifier) Send(message UnifiedMessage) error {
	// 这里放实际发送逻辑
	if !shared.EnableWebhookNotifications {
		return nil
	}
	fmt.Printf("[Email] %s\n", message)
	return nil
}
