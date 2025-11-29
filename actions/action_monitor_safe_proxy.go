package actions

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"slices"
	"sync"

	"encoding/hex"

	"github.com/Zellic/EVM-trackooor/shared"
	"github.com/Zellic/EVM-trackooor/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// -------------------------
// 配置 Safe 常用事件签名（修复RemoveOwner参数配置）
// -------------------------

var enableNotifications = true

type EventMeta struct {
	Name     string
	Params   []EventParam
	Decimals map[string]uint8
}

type EventParam struct {
	Type    string
	Name    string
	Indexed bool // RemoveOwner的owner参数为非indexed，存于Data
}

var eventSignatures = map[string]EventMeta{
	// ERC20 Transfer事件
	//crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)")).Hex(): {
	//	Name: "Transfer",
	//	Params: []EventParam{
	//		{Type: "address", Name: "from", Indexed: true},
	//		{Type: "address", Name: "to", Indexed: true},
	//		{Type: "uint256", Name: "value", Indexed: false},
	//	},
	//	Decimals: map[string]uint8{
	//		"0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C97": 8,
	//		"0xdAC17F958D2ee523a2206206994597C13D831ec": 6,
	//		"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB4": 6,
	//	},
	//},
	// Safe AddOwner事件（owner为indexed，存于Topics）
	"0x9465fa0c962cc76958e6373a993326400c1c94f8be2fe3a952adfa7f60b2ea26": {
		Name: "AddOwner",
		Params: []EventParam{
			{Type: "address", Name: "owner", Indexed: true},
		},
	},
	// 修复：Safe RemoveOwner事件（owner为非indexed，存于Data）
	"0xf8d49fc529812e9a7c5c50e69c20f0dccc0db8fa95c98bc58cc9a4f1c1299eaf": {
		Name: "RemoveOwner",
		Params: []EventParam{
			{Type: "address", Name: "owner", Indexed: false}, // 关键修复：Indexed=false
		},
	},
	// Safe ChangeThreshold事件
	"0x610f7ff2b304ae8903c3de74c60c6ab1f7d6226b3f52c5161905bb5ad4039c93": {
		Name: "ChangeThreshold",
		Params: []EventParam{
			{Type: "uint256", Name: "newThreshold", Indexed: false},
		},
	},
	// ExecutionSuccess事件
	crypto.Keccak256Hash([]byte("ExecutionSuccess(bytes32,uint256)")).Hex(): {
		Name: "ExecutionSuccess",
		Params: []EventParam{
			{Type: "bytes32", Name: "txHash", Indexed: false},
			{Type: "uint256", Name: "payment", Indexed: false},
		},
	},
}

// -------------------------
// 全局变量（新增日志去重缓存）
// -------------------------
var (
	monitoredProxyAddresses []common.Address
	logCache                sync.Map // 缓存已处理的日志：key=txHash-eventID-index，value=struct{}{}
)

// -------------------------
// Action信息（不变）
// -------------------------
func (actionInfo) InfoMonitorSafeProxy() actionInfo {
	name := "MonitorSafeProxy"
	overview := `Monitors Gnosis Safe proxy transactions and detects potentially dangerous function calls.`

	description := `This action listens for Safe proxy execTransaction() calls. 
When detected, it retrieves all internal transactions/logs to analyze the executed call. 
If any call involves admin role transfer, ownership change, or token transfer, 
it triggers a warning log.`

	options := `"addresses" - array of known Safe proxy addresses to monitor.`

	example := `"MonitorSafeProxy": {
	"addresses": [
		"0x1234...SafeAddress",
		"0xabcd...SafeAddress2"
	]
}`

	return actionInfo{
		ActionName:          name,
		ActionOverview:      overview,
		ActionDescription:   description,
		ActionOptionDetails: options,
		ActionConfigExample: example,
	}
}

// -------------------------
// 初始化监控（不变）
// -------------------------
func (p action) InitMonitorSafeProxy() {
	monitoredProxyAddresses = p.o.Addresses
	for _, addr := range monitoredProxyAddresses {
		addTxAddressAction(addr, handleProxyAddressTx)
	}
	// 如果 CustomOptions 存在，则尝试读取
	if p.o.CustomOptions != nil {
		if v, ok := p.o.CustomOptions["enableNotifications"].(bool); ok {
			enableNotifications = v
		}
	}

	fmt.Printf("[SAFE] Email notifications: %v\n",
		enableNotifications)
}

// -------------------------
// 每笔交易处理（不变）
// -------------------------
func handleProxyAddressTx(p ActionTxData) {
	to := *p.To
	inputData := p.Transaction.Data()

	if !slices.Contains(monitoredProxyAddresses, to) {
		return
	}

	if len(inputData) < 4 {
		return
	}

	fmt.Printf("\n[SAFE] Safe Proxy Wallet is called in block %v by %v -> %v\n",
		p.Block.Number().Uint64(), p.From.Hex(), to.Hex())
	fmt.Printf("[SAFE] Transaction Hash: %v\n", p.Transaction.Hash().Hex())

	receipt, err := shared.Client.TransactionReceipt(context.Background(), p.Transaction.Hash())
	if err != nil {
		log.Println("TxReceipt error:", err)
		return
	}

	// 遍历 receipt.Logs
	for _, l := range receipt.Logs {
		analyzeSafeLog(*l)
	}
}

// -------------------------
// 日志解析函数（新增去重+修复参数读取）
// -------------------------
func analyzeSafeLog(l types.Log) {
	// 1. 日志去重：避免重复处理同一日志
	logKey := fmt.Sprintf("%s-%s-%d", l.TxHash.Hex(), l.Topics[0].Hex(), l.Index)
	if _, exists := logCache.Load(logKey); exists {
		return
	}
	logCache.Store(logKey, struct{}{})
	// 2. 匹配事件签名
	eventID := l.Topics[0].Hex()
	eventMeta, ok := eventSignatures[eventID]
	if !ok {
		return
	}

	// 3. 输出事件基本信息
	fmt.Printf("[SAFE] Detected Event: %s (Contract: %s, TxHash: %s, LogIndex: %d)\n",
		eventMeta.Name, l.Address.Hex(), l.TxHash.Hex(), l.Index)
	if shared.EnableWebhookNotifications || shared.EnableEmailNotifications {
		//notify.SendMessage(
		//	notify.NewUnifiedMessage("safe_proxy", fmt.Sprintf("[SAFE] Detected Event: %s (Contract: %s, TxHash: %s, LogIndex: %d)",
		//		eventMeta.Name, l.Address.Hex(), l.TxHash.Hex(), l.Index), l.Address.Hex()))
	}
	// 4. 解析参数（修复：区分indexed/非indexed存储位置）
	topicsIndex := 1 // Topics[0]是事件签名，索引参数从Topics[1]开始
	dataIndex := 0   // 非索引参数从Data[0]开始，按32字节切片读取

	for _, param := range eventMeta.Params {
		var valueStr string

		switch {
		// 处理indexed参数（存于Topics）
		case param.Indexed:
			if topicsIndex >= len(l.Topics) {
				valueStr = "invalid (insufficient topics)"
				break
			}
			topicBytes := l.Topics[topicsIndex].Bytes()
			valueStr = parseParamValue(param.Type, topicBytes, l.Address, eventMeta.Decimals)
			topicsIndex++

		// 处理非indexed参数（存于Data，修复RemoveOwner参数读取）
		case !param.Indexed:
			// 以太坊ABI编码：非indexed参数按32字节对齐存储
			if len(l.Data) < dataIndex+32 {
				valueStr = "invalid (insufficient data)"
				break
			}
			dataBytes := l.Data[dataIndex : dataIndex+32]
			valueStr = parseParamValue(param.Type, dataBytes, l.Address, eventMeta.Decimals)
			dataIndex += 32
		}

		// 输出参数
		fmt.Printf("  - %s (%s): %s\n", param.Name, param.Type, valueStr)
	}
}

// -------------------------
// 参数解析工具（不变，适配address类型）
// -------------------------
func parseParamValue(paramType string, data []byte, tokenAddr common.Address, decimalsMap map[string]uint8) string {
	switch paramType {
	case "address":
		// 无论indexed/非indexed，address在ABI中均为32字节（前12字节补0），取后20字节
		return common.BytesToAddress(data[12:]).Hex()

	case "uint256":
		value := new(big.Int).SetBytes(data)
		if decimalsMap != nil {
			if decimals, ok := decimalsMap[tokenAddr.Hex()]; ok {
				return utils.FormatDecimals(value, decimals)
			}
			return utils.FormatDecimals(value, 18)
		}
		return value.String()

	case "bytes32":
		return common.BytesToHash(data).Hex()

	default:
		return fmt.Sprintf("0x%s", hex.EncodeToString(data))
	}
}
