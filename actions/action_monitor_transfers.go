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

const NULLAddress string = "0x0000000000000000000000000000000000000000"

type TxTransferInfo struct {
	TotalUSD       float64   // 累计转出金额
	Created        time.Time // 记录第一次出现该交易的时间
	TokenAddresses []common.Address
	FromAddress    common.Address
	ToAddress      common.Address
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
		],
		"auto-fetch-monitor-address-chain-id": "bsc",
		"auto-fetch-monitor-token-address-chain-name": "binance-smart-chain",
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
	autoFetchByChainId, ok := p.o.CustomOptions["auto-fetch-monitor-address-chain-id"].(string)
	if !ok {
		monitoredAddresses = p.o.Addresses
	} else {
		monitoredAddressesStrings, err := market.GetBinanceFuturesTokenAddressList(autoFetchByChainId)
		monitoredAddresses = make([]common.Address, 0, len(monitoredAddressesStrings))
		for _, addr := range monitoredAddressesStrings {
			monitoredAddresses = append(monitoredAddresses, common.HexToAddress(addr))
		}
		if err != nil {
			panic(fmt.Sprintf("fetchTopProjectsAddressByChainID failed: %v", err))
		}
	}
	fmt.Printf("Monitor transfers: monitored %v addresses\n", len(monitoredAddresses))
	//for _, addr := range monitoredAddresses {
	//	addTxAddressAction(addr, handleAddressTx)
	//}
	// monitor erc20 token for transfer events
	var erc20TokenAddresses []common.Address
	autoFetchTokenChianName, ok := p.o.CustomOptions["auto-fetch-monitor-token-chian-name"].(string)
	if ok {
		erc20TokenAddressesString, err := market.GetMonitTokenAddressesByChainID(autoFetchTokenChianName)
		erc20TokenAddresses = make([]common.Address, 0, len(erc20TokenAddressesString))
		if err != nil {
			panic(fmt.Sprintf("fetchTopProjectsAddressByChainID failed: %v", err))
		}
		for _, addr := range erc20TokenAddressesString {
			erc20TokenAddresses = append(erc20TokenAddresses, common.HexToAddress(addr))
		}
	} else {
		erc20TokenInterface := p.o.CustomOptions["erc20-tokens"].([]interface{})
		erc20TokenAddresses = make([]common.Address, 0, len(erc20TokenInterface))
		for _, addr := range erc20TokenInterface {
			erc20TokenAddresses = append(erc20TokenAddresses, common.HexToAddress(addr.(string)))
		}
	}
	fmt.Printf("Monitor %v erc20TokenAddresses\n", len(erc20TokenAddresses))
	for _, erc20TokenAddress := range erc20TokenAddresses {
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
				// 1️⃣ 超过 1 秒且净流出超过 500000 USD → 触发警报
				if info.TotalUSD < -500000 && age > time.Second {
					var exploiterAddress common.Address
					var attackedAddress common.Address
					if slices.Contains(monitoredAddresses, info.FromAddress) {
						exploiterAddress = info.ToAddress
						attackedAddress = info.FromAddress
					} else {
						exploiterAddress = info.FromAddress
						attackedAddress = info.ToAddress
					}
					fmtMessage := fmt.Sprintf("🚨 ALERT: Address %v is attacked by exploiterAddress %v Tx %v net outflow %.2f USD (created %v, %v ago)",
						attackedAddress.Hex(), exploiterAddress.Hex(), hash.Hex(), info.TotalUSD, info.Created.Format(time.RFC3339),
						age.Round(time.Millisecond))
					fmt.Printf(fmtMessage)
					// 根据holderAddress查找到代币地址
					var coinId string
					if attackedAddress.Hex() != NULLAddress {
						coinId, err = market.GetCoinIdByProjectAddress(attackedAddress.Hex())
						if err != nil {
							fmt.Printf("Failed to fetch coinId for address %v: %v\n", attackedAddress.Hex(), err)
							continue
						}
					} else {
						coinId = info.TokenAddresses[0].Hex()
					}
					fmt.Printf("CoinId for address %v is %v\n", attackedAddress.Hex(), coinId)
					notify.SendMessage(
						notify.NewUnifiedMessage("transfers", fmtMessage, coinId))
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
//func handleAddressTx(p ActionTxData) {
//	from := *p.From
//	to := *p.To
//	value := p.Transaction.Value()
//	// tx is from monitored address (since it can be either from/to)
//	// and value of tx > 0
//	if (slices.Contains(monitoredAddresses, from) || slices.Contains(monitoredAddresses, to)) &&
//		value.Cmp(big.NewInt(0)) > 0 {
//		// alert
//		fmt.Printf("Native ETH Transfer from %v to %v with value %v ETH in tx %v\n", from, to, utils.FormatDecimals(value, 18), p.Transaction.Hash().Hex())
//	}
//}

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
					TotalUSD:       0,
					Created:        time.Now(),
					TokenAddresses: []common.Address{token},
					FromAddress:    from,
					ToAddress:      to,
				}
			}
			if isSender {
				entry.TotalUSD -= totalValue
			}
			if isReceiver {
				entry.TotalUSD += totalValue
			}
			txTransferTotalsMu.Lock()
			defer txTransferTotalsMu.Unlock()
			txTransferTotals[hash] = entry
			//fmt.Printf("ERC20 Transfer from %v to %v with value %v %v (%.2f)USD in tx %v\n", from, to, utils.FormatDecimals(value, decimals), symbol, totalValue, hash.Hex())
		} else {
			fmt.Printf("ERC20 Transfer from %v to %v with value %v %v (price err: %v) in tx %v\n", from, to, utils.FormatDecimals(value, decimals), symbol, err, hash.Hex())
		}
	}
}
