package market

import (
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// TokenPriceInUSD 获取代币的美元价格
func TokenPriceInUSD(token common.Address) (float64, error) {
	// 转为小写字符串地址
	price, err := GetTokenPriceByContract(strings.ToLower(token.Hex()))
	if err != nil {
		return 0, err
	}

	if price <= 0 {
		return 0, fmt.Errorf("invalid price received: %f", price)
	}

	return price, nil
}

func GetBinanceFuturesTokenAddressList(chainId string) ([]string, error) {
	// 遍历，直到返回值为空
	var tokenAddressList []string
	limit := 25
	offset := 0

	// 创建一个map来存储地址和对应的总余额
	addressBalanceMap := make(map[string]float64)

	for {
		response, isEnd, err := fetchTopProjectsAddressByChainID(chainId, limit, offset)
		if err != nil {
			return nil, err
		}

		// 处理响应数据，计算每个地址的总余额
		for _, contractExchange := range response.ContractExchanges {
			if contractExchange.Coin.CurrentPrice != nil {
				for _, holding := range contractExchange.Coin.Holdings {
					// 只处理指定链类型的地址
					if holding.Holder.ChainType == chainId && holding.Balance != nil {
						// 计算当前代币余额乘以当前价格，得到账户余额
						accountBalance := *holding.Balance * *contractExchange.Coin.CurrentPrice
						// 累加到对应地址的总余额中
						addressBalanceMap[holding.Holder.Address] += accountBalance
					}
				}
			}
		}

		if isEnd {
			break
		}
		offset += limit
	}

	// 根据总余额筛选地址（大于10万美元且小于1万亿美元）
	for addr, balance := range addressBalanceMap {
		if balance >= 1000000 && balance < 1e12 {
			tokenAddressList = append(tokenAddressList, addr)
		}
	}

	if len(tokenAddressList) == 0 {
		return nil, fmt.Errorf("no address found for chainId: %s", chainId)
	}

	return tokenAddressList, nil
}

func GetMonitTokenAddressesByChainID(chainId string) ([]string, error) {
	var tokenAddressList []string
	limit := 25
	offset := 0

	for {
		response, isEnd, err := fetchMonitTokenAddressesByChainID(chainId, limit, offset)
		if err != nil {
			return nil, err
		}

		tokenAddressList = append(tokenAddressList, response...)

		if isEnd {
			break
		}
		offset += limit
	}

	return tokenAddressList, nil
}

func GetCoinIdByProjectAddress(projectAddress string) (string, error) {
	return fetchCoinIdByProjectAddress(projectAddress)
}
