package notify

// -------------------------
// 消息格式化示例
// -------------------------

// UnifiedMessage 统一格式的消息结构体
type UnifiedMessage struct {
	TriggerType  string `json:"triggerType"`
	Message      string `json:"message"`
	TokenAddress string `json:"tokenAddress"`
}

// NewUnifiedMessage 创建一个新的统一格式消息
func NewUnifiedMessage(triggerType, message, tokenAddress string) UnifiedMessage {
	return UnifiedMessage{
		TriggerType:  triggerType,
		Message:      message,
		TokenAddress: tokenAddress,
	}
}
