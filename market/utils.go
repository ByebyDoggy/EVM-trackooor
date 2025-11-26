package market

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// TokenPriceInUSD 获取代币的美元价格
func TokenPriceInUSD(token common.Address) (float64, error) {
	price, err := GetTokenPriceByContract(token.Hex())
	if err != nil {
		return 0, err
	}

	if price <= 0 {
		return 0, fmt.Errorf("invalid price received: %f", price)
	}

	return price, nil
}
