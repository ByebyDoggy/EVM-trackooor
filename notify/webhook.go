package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)
import "github.com/Zellic/EVM-trackooor/shared"

// -------------------------
// Webhook 通道示例
// -------------------------
type WebhookNotifier struct {
	// 可以加 URL 等字段
}

func (w *WebhookNotifier) Send(message UnifiedMessage) error {
	// 这里放实际 HTTP POST 逻辑
	if !shared.EnableWebhookNotifications {
		return nil
	}
	fmt.Printf("[Webhook] %s\n", message)

	// 将UnifiedMessage转换为JSON格式
	jsonData, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal message to JSON: %v", err)
	}

	resp, err := http.Post(
		shared.Options.WebHookURL,
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return fmt.Errorf("webhook post failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("webhook read response failed: %w", err)
	}
	fmt.Printf("[Webhook] resp: %s\n", string(body))
	return nil
}
