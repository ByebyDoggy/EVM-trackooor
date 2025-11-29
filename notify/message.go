package notify

// -------------------------
// 消息格式化示例
// -------------------------

// UnifiedMessage 统一格式的消息结构体

type TokenInfo struct {
	TokenAddress string `json:"tokenAddress"`
	TokenSymbol  string `json:"tokenSymbol"`
	TokenName    string `json:"tokenName"`
}

type UnifiedMessage struct {
	TriggerType string `json:"triggerType"`
	Message     string `json:"message"`
	TokenInfos  []TokenInfo
}

// NewUnifiedMessage 创建一个新的统一格式消息
func NewUnifiedMessage(triggerType string, message string, tokenInfos []TokenInfo) UnifiedMessage {
	return UnifiedMessage{
		TriggerType: triggerType,
		Message:     message,
		TokenInfos:  tokenInfos,
	}
}
