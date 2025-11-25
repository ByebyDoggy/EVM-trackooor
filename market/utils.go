package market

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

func TokenPriceInUSD(token common.Address) (float64, error) {
	coinInfos, err := GetCoinByContract(token.Hex())
	if err != nil {
		return 0, err
	}
	if len(coinInfos) == 0 {
		return 0, fmt.Errorf("no coin info found")
	}
	for _, coinInfo := range coinInfos {
		if coinInfo.SupplyInfo == nil || coinInfo.SupplyInfo.CachedPrice == nil {
			continue
		}
		return *coinInfo.SupplyInfo.CachedPrice, nil
	}
	return 0, fmt.Errorf("no cached price found")
}
