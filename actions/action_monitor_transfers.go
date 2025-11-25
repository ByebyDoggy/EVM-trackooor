package actions

import (
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/Zellic/EVM-trackooor/market"
	"github.com/Zellic/EVM-trackooor/notify"
	"github.com/Zellic/EVM-trackooor/shared"
	"github.com/Zellic/EVM-trackooor/utils"
	"github.com/ethereum/go-ethereum/common"
)

type TxTransferInfo struct {
	TotalUSD     float64   // 累计转出金额
	Created      time.Time // 记录第一次出现该交易的时间
	TokenAddress common.Address
}

var (
	txTransferTotalsMu sync.Mutex
	txTransferTotals   = make(map[common.Hash]TxTransferInfo)
)

func (actionInfo) InfoMonitorTransfers() actionInfo {
	name := "MonitorTransfers"
	overview := `Monitors native ETH and ERC20 token transfers made by specified addresses, 
and triggers alerts whenever monitored addresses send out ETH or tracked ERC20 tokens.`

	description := `This action listens for:
1. Native ETH transfers originating from the monitored addresses.
2. ERC20 Transfer(address,address,uint256) events emitted by specified ERC20 tokens, 
   where the "from" address is one of the monitored addresses.

When a matching transfer is detected, details are printed to the console or logged.`

	options := `"erc20-tokens" - array of ERC20 token contract addresses to monitor transfer events for.`

	example := `"MonitorTransfers": {
	"addresses": {
		"0x742d35Cc6634C0532925a3b844Bc454e4438f44e",
		"0x66f820a414680B5bcda5eECA5dea238543F42054"
	},
	"options": {
		"erc20-tokens": [
			"0xdac17f958d2ee523a2206206994597c13d831ec7":{},
			"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48":{}
		]
	}
}`

	return actionInfo{
		ActionName:          name,
		ActionOverview:      overview,
		ActionDescription:   description,
		ActionOptionDetails: options,
		ActionConfigExample: example,
	}
}

var monitoredAddresses []common.Address

func (p action) InitMonitorTransfers() {
	// monitor addresses for transactions
	monitoredAddresses = p.o.Addresses
	for _, addr := range monitoredAddresses {
		addTxAddressAction(addr, handleAddressTx)
	}
	// monitor erc20 token for transfer events
	for _, addrInterface := range p.o.CustomOptions["erc20-tokens"].([]interface{}) {
		erc20TokenAddress := common.HexToAddress(addrInterface.(string))
		addAddressEventSigAction(erc20TokenAddress, "Transfer(address,address,uint256)", handleTokenTransfer)
	}
	if market.BaseCoinMarketURL == "" {
		fmt.Println("BaseCoinMarketURL not set")
	}
	response, err := market.HealthCheck()
	if err != nil {
		panic(fmt.Sprintf("Market HealthCheck failed: %v", err))
	}
	fmt.Println("Market HealthCheck response:", response)
	// 监控净流出数据
	go func() {
		for {
			time.Sleep(1 * time.Second)

			txTransferTotalsMu.Lock()
			now := time.Now()

			for hash, info := range txTransferTotals {
				age := now.Sub(info.Created)
				tokenAddress := info.TokenAddress
				// 1️⃣ 超过 1 秒且净流出超过 10000 USD → 触发警报
				if info.TotalUSD < -10000 && age > time.Second {
					fmtMessage := fmt.Sprintf("🚨 ALERT: Token %v Tx %v net outflow %.2f USD (created %v, %v ago)",
						tokenAddress.Hex(), hash.Hex(), info.TotalUSD, info.Created.Format(time.RFC3339),
						age.Round(time.Millisecond))
					fmt.Printf(fmtMessage)

					notify.SendMessage(
						notify.NewUnifiedMessage("transfers", fmtMessage, tokenAddress.Hex()))
					// 警报触发后，删除该记录（避免重复触发）
					delete(txTransferTotals, hash)
					continue
				}

				// 2️⃣ 超时清理：超过 5 分钟未触发警报的交易自动删除
				if age > 5*time.Minute {
					delete(txTransferTotals, hash)
				}
			}

			txTransferTotalsMu.Unlock()
		}
	}()

}

// called when a tx is from/to monitored address
func handleAddressTx(p ActionTxData) {
	from := *p.From
	to := *p.To
	value := p.Transaction.Value()
	// tx is from monitored address (since it can be either from/to)
	// and value of tx > 0
	if (slices.Contains(monitoredAddresses, from) || slices.Contains(monitoredAddresses, to)) &&
		value.Cmp(big.NewInt(0)) > 0 {
		// alert
		fmt.Printf("Native ETH Transfer from %v to %v with value %v ETH in tx %v\n", from, to, utils.FormatDecimals(value, 18), p.Transaction.Hash().Hex())
	}
}

// called when erc20 token we're tracking emits Transfer event
func handleTokenTransfer(p ActionEventData) {
	from := p.DecodedTopics["from"].(common.Address)
	to := p.DecodedTopics["to"].(common.Address)
	value := p.DecodedData["value"].(*big.Int)
	// erc20 transfer is from an address we're monitoring
	isSender := slices.Contains(monitoredAddresses, from)
	isReceiver := slices.Contains(monitoredAddresses, to)
	if isSender || isReceiver {
		// get erc20 token info (to format value decimals + symbol)
		token := p.EventLog.Address
		tokenInfo := shared.RetrieveERC20Info(token)
		decimals := tokenInfo.Decimals
		symbol := tokenInfo.Symbol
		hash := p.EventLog.TxHash
		// alert
		if price, err := market.TokenPriceInUSD(token); err == nil {
			// Convert the value to a float64 for price calculation
			valueFloat := utils.FormatDecimalsToFloat(value, decimals)
			totalValue := price * valueFloat
			entry, exists := txTransferTotals[hash]
			if !exists { // 没有就初始化
				entry = TxTransferInfo{
					TotalUSD:     0,
					Created:      time.Now(),
					TokenAddress: token,
				}
			}
			if isSender {
				entry.TotalUSD -= totalValue
			}
			if isReceiver {
				entry.TotalUSD += totalValue
			}
			txTransferTotals[hash] = entry
			fmt.Printf("ERC20 Transfer from %v to %v with value %v %v (%.2f)USD in tx %v\n", from, to, utils.FormatDecimals(value, decimals), symbol, totalValue, hash.Hex())
		} else {
			fmt.Printf("ERC20 Transfer from %v to %v with value %v %v (price err: %v) in tx %v\n", from, to, utils.FormatDecimals(value, decimals), symbol, err, hash.Hex())
		}
	}
}
