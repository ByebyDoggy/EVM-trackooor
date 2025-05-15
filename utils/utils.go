package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zellic/EVM-trackooor/contracts/IERC20Metadata"
	"github.com/Zellic/EVM-trackooor/contracts/UniswapV3Factory"
	"github.com/Zellic/EVM-trackooor/contracts/uniswapPool"
	"github.com/Zellic/EVM-trackooor/shared"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// returns whether a tx is a deployment or not
// useful instead of `determineTxType` as it does not require a query
func IsDeploymentTx(tx *types.Transaction) bool {
	return tx.To() == nil
}

func RemoveDuplicates[T comparable](sliceList []T) []T {
	allKeys := make(map[T]bool)
	list := []T{}
	for _, item := range sliceList {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

// return current / latest block number
func GetLatestBlockNum() *big.Int {
	header, _ := shared.Client.HeaderByNumber(context.Background(), nil)
	latestBlockNum := header.Number
	return latestBlockNum
}

// returns who sent the transaction
func GetTxSender(tx *types.Transaction) *common.Address {
	// TODO get system tx sender properly
	// prevent crashing when tx type is unsupported like for l2 chains
	v, r, s := tx.RawSignatureValues()
	zero := big.NewInt(0)
	if v == nil || r == nil || s == nil {
		shared.Infof(slog.Default(), "system/invalid tx type handled\n")
		return &common.Address{}
	}
	if v.Cmp(zero) == 0 && r.Cmp(zero) == 0 && s.Cmp(zero) == 0 {
		shared.Infof(slog.Default(), "system/invalid tx type handled\n")
		return &common.Address{}
	}

	newSignerFuncs := []func(*big.Int) types.Signer{
		types.NewCancunSigner,
		types.NewLondonSigner,
		types.NewEIP2930Signer,
	}
	for _, newSignerFunc := range newSignerFuncs {
		from, err := types.Sender(newSignerFunc(shared.ChainID), tx)
		if err == nil {
			return &from
		}
	}

	from, err := types.NewEIP155Signer(shared.ChainID).Sender(tx)
	if err != nil {
		log.Fatalf("Could not get sender (from) of transaction: %v\n", err)
	}
	return &from
}

// fetch file from URL
func FetchFileBytes(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error fetching the file: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error: received status code %d from %s", resp.StatusCode, url)
	}

	fileBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response body: %v", err)
	}

	return fileBytes, nil
}

func ComputeDecimal(n *big.Int, decimals uint8) *big.Float {
	r := big.NewFloat(0).SetInt(n)
	q := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	r = big.NewFloat(0).Quo(r, big.NewFloat(0).SetInt(q))
	return r
}

func DecodeSqrtX96Price(sqrtPriceX96 *big.Int) *big.Float {
	sqrtPriceX96Float := new(big.Float).SetPrec(512).SetInt(sqrtPriceX96)
	xSquared := new(big.Float).Mul(sqrtPriceX96Float, sqrtPriceX96Float)
	twoPower192 := new(big.Float).SetPrec(256).SetInt(new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil))
	price := new(big.Float).Quo(xSquared, twoPower192)
	return price
}

func AbsFloat64(n float64) float64 {
	if n < 0 {
		return -n
	}
	return n
}

// contract USD price, using coin API

type AddressBalanceRetriever struct {
	// map token symbol to its price (USD) for 1 token
	TokenRates      map[string]float64
	TokenRatesMutex sync.RWMutex
	// map token address to its info (decimals, symbol, etc.)
	TokenInfos      map[common.Address]shared.ERC20Info
	TokenInfosMutex sync.RWMutex
	// www.coinapi.io api key
	coinApiKey string
	// timestamp the price was last updated
	LastUpdated time.Time
}

// returns AddressBalanceRetriever
// can be used to get USD balance of an address,
// which is native ETH value + ERC20 token value
// you specify which ERC20 tokens to check balance for
// uses coinAPI to get token price, not all ERC20 tokens will have price data
func NewAddressBalanceRetriever(coinApiKey string, tokens []common.Address) *AddressBalanceRetriever {
	_tokenRates := make(map[string]float64)
	_tokenInfos := make(map[common.Address]shared.ERC20Info)

	abr := &AddressBalanceRetriever{
		TokenRates: _tokenRates,
		TokenInfos: _tokenInfos,
		coinApiKey: coinApiKey,
	}

	// get token Infos
	for _, tokenAddress := range tokens {
		erc20Info := shared.RetrieveERC20Info(tokenAddress)
		abr.TokenInfosMutex.Lock()
		abr.TokenInfos[tokenAddress] = erc20Info
		abr.TokenInfosMutex.Unlock()
	}

	// set tokenRates
	abr.UpdateTokenPrices()

	// fmt.Printf("TokenRates: %+v\n", abr.TokenRates)

	return abr
}

// calculates balance of contract, in USD, balance = native ETH bal + token bal
func (a *AddressBalanceRetriever) GetAddressUSDbalance(contract common.Address, blockNum *big.Int) float64 {
	var totalUSD float64

	prec := int64(6)

	// native ETH
	wei, _ := shared.Client.BalanceAt(context.Background(), contract, blockNum)
	divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18-prec), nil)
	ether, _ := wei.Div(wei, divisor).Float64()
	ether = ether / math.Pow(10, float64(prec))

	a.TokenRatesMutex.RLock()
	totalUSD += ether * a.TokenRates["ETH"]
	a.TokenRatesMutex.RUnlock()
	if totalUSD != 0 {
		fmt.Printf("totalUSD: %v\n", totalUSD)
		fmt.Printf("ether: %v\n", ether)
	}

	// tokens
	a.TokenInfosMutex.Lock()
	defer a.TokenInfosMutex.Unlock()
	for tokenAddress, tokenInfo := range a.TokenInfos {
		balance, err := GetERC20BalanceOf(tokenAddress, contract, blockNum)
		if err != nil {
			log.Printf("AddressBalanceRetriever - Error when getting erc20 balance of token %v! err %v\n", tokenAddress, err)
			continue
		}
		decimals := tokenInfo.Decimals
		symbol := tokenInfo.Symbol
		symbol = strings.ToUpper(symbol) // coin API uses uppercase symbols

		divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals)-prec), nil)
		amount, _ := wei.Div(balance, divisor).Float64()
		amount = amount / math.Pow(10, float64(prec))

		a.TokenRatesMutex.RLock()
		totalUSD += amount * a.TokenRates[symbol]
		a.TokenRatesMutex.RUnlock()
		if amount != 0 {
			// fmt.Printf("amount: %v\n", amount)
			// fmt.Printf("symbol: %v\n", symbol)
			// fmt.Printf("balance: %v\n", balance)
			// fmt.Printf("totalUSD: %v\n", totalUSD)
		}
	}

	return totalUSD
}

// calculates USD value of ETH given amount of ETH
// returns 0 if doesnt have info for token
func (a *AddressBalanceRetriever) GetEthUSDValue(balance *big.Int) float64 {
	var totalUSD float64

	prec := int64(6)

	divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18-prec), nil)
	ether, _ := big.NewInt(0).Div(balance, divisor).Float64()
	ether = ether / math.Pow(10, float64(prec))

	a.TokenRatesMutex.RLock()
	totalUSD += ether * a.TokenRates["ETH"]
	a.TokenRatesMutex.RUnlock()

	return totalUSD
}

// calculates USD value given token address and amount of tokens
// returns 0 if doesnt have info for token
func (a *AddressBalanceRetriever) GetTokenUSDValue(token common.Address, balance *big.Int) float64 {
	var totalUSD float64

	tokenInfo, ok := a.TokenInfos[token]
	if !ok {
		log.Printf("AddressBalanceRetriever - No tokenInfo data for token %v\n", token)
		return 0
	}

	decimals := tokenInfo.Decimals
	symbol := tokenInfo.Symbol
	symbol = strings.ToUpper(symbol)

	prec := int64(6)

	divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals)-prec), nil)
	amount, _ := big.NewInt(0).Div(balance, divisor).Float64()
	amount = amount / math.Pow(10, float64(prec))

	a.TokenRatesMutex.RLock()
	totalUSD += amount * a.TokenRates[symbol]
	a.TokenRatesMutex.RUnlock()

	return totalUSD
}

func GetERC20BalanceOf(token common.Address, address common.Address, blockNum *big.Int) (*big.Int, error) {
	tokenInstance, err := IERC20Metadata.NewIERC20Metadata(token, shared.Client)
	if err != nil {
		panic(err)
	}
	balance, err := tokenInstance.BalanceOf(&bind.CallOpts{BlockNumber: blockNum}, address)
	if err != nil {
		return big.NewInt(0), err
	}
	return balance, nil
}

// token prices

type Rate struct {
	Time         string  `json:"time"`
	AssetIDQuote string  `json:"asset_id_quote"`
	Rate         float64 `json:"rate"`
}

type CurrentRatesData struct {
	AssetIDBase string `json:"asset_id_base"`
	Rates       []Rate `json:"rates"`
}

// func (a *AddressBalanceRetriever) UpdateTokenPrices() {

// 	url := "https://rest.coinapi.io/v1/exchangerate/USD?invert=true"
// 	method := "GET"

// 	client := &http.Client{}
// 	req, err := http.NewRequest(method, url, nil)

// 	if err != nil {
// 		log.Printf("AddressBalanceRetriever - err when update token prices: %v\n", err)
// 		return
// 	}
// 	req.Header.Add("Accept", "text/plain")
// 	req.Header.Add("X-CoinAPI-Key", a.coinApiKey)

// 	res, err := client.Do(req)
// 	if err != nil {
// 		log.Printf("AddressBalanceRetriever - err when update token prices: %v\n", err)
// 		return
// 	}
// 	defer res.Body.Close()

// 	body, err := io.ReadAll(res.Body)
// 	if err != nil {
// 		log.Printf("AddressBalanceRetriever - err when update token prices: %v\n", err)
// 		return
// 	}

// 	if strings.Contains(string(body), "Forbidden") {
// 		log.Fatalf("AddressBalanceRetriever - could not retrieve token prices: body: %v", string(body))
// 	}

// 	var currentRates CurrentRatesData
// 	json.Unmarshal(body, &currentRates)

// 	for _, rate := range currentRates.Rates {
// 		a.TokenRatesMutex.Lock()
// 		a.TokenRates[rate.AssetIDQuote] = rate.Rate
// 		a.TokenRatesMutex.Unlock()
// 	}

// 	log.Printf("AddressBalanceRetriever - Updated token prices for %v tokens\n", len(a.TokenRates))
// 	a.LastUpdated = time.Now()
// }

func (a *AddressBalanceRetriever) UpdateTokenPrices() {

	for _, tokenInfo := range a.TokenInfos {
		symbol := strings.ToUpper(tokenInfo.Symbol)
		price, err := a.GetTokenUsdPrice(symbol)
		if err != nil {
			log.Printf("AddressBalanceRetriever - could not get price for %v\n", symbol)
			continue
		}
		log.Printf("AddressBalanceRetriever - %v $%v USD\n", symbol, price)

		a.TokenRatesMutex.Lock()
		a.TokenRates[symbol] = price
		a.TokenRatesMutex.Unlock()
	}

	log.Printf("AddressBalanceRetriever - Updated token prices for %v tokens\n", len(a.TokenRates))
	a.LastUpdated = time.Now()
}

type ExchangeRateResponse struct {
	Time         string  `json:"time"`
	AssetIDBase  string  `json:"asset_id_base"`
	AssetIDQuote string  `json:"asset_id_quote"`
	Rate         float64 `json:"rate"`
}

func (a *AddressBalanceRetriever) GetTokenUsdPrice(symbol string) (float64, error) {
	url := fmt.Sprintf("https://rest.coinapi.io/v1/exchangerate/%s/USD", symbol)

	client := &http.Client{}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CoinAPI-Key", a.coinApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	if strings.Contains(string(body), "Forbidden") {
		log.Fatalf("AddressBalanceRetriever - could not retrieve token prices: body: %v", string(body))
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed to fetch data: %s", body)
	}

	var exchangeRateResponse ExchangeRateResponse
	if err := json.Unmarshal(body, &exchangeRateResponse); err != nil {
		return 0, err
	}

	return exchangeRateResponse.Rate, nil
}

// address USD prices, using Binance and Uniswap APIs

type AddressBalanceQuery struct {
	// map token symbol to its price (USD) for 1 token
	TokenRates      map[string]float64
	TokenRatesMutex sync.RWMutex
	// map token address to its info (decimals, symbol, etc.)
	TokenInfos      map[common.Address]shared.ERC20Info
	TokenInfosMutex sync.RWMutex

	// used as base tokens for uniswap pool
	CommonPrices []tokenPrice

	UniswapV3FactoryAddress common.Address

	LastUpdated time.Time
}

type tokenPrice struct {
	Symbol  string
	Address common.Address
	Price   float64
}

func NewAddressBalanceQuery(
	tokenAddresses []common.Address,
	UniswapV3FactoryAddress common.Address,
) *AddressBalanceQuery {
	_tokenRates := make(map[string]float64)
	_tokenInfos := make(map[common.Address]shared.ERC20Info)

	abq := &AddressBalanceQuery{
		UniswapV3FactoryAddress: UniswapV3FactoryAddress,
		TokenRates:              _tokenRates,
		TokenInfos:              _tokenInfos,
	}

	// get token Infos
	fmt.Printf("abq - getting token infos\n")
	for _, tokenAddress := range tokenAddresses {
		tokenInfo := shared.RetrieveERC20Info(tokenAddress)
		abq.TokenInfosMutex.Lock()
		abq.TokenInfos[tokenAddress] = tokenInfo
		abq.TokenInfosMutex.Unlock()

		// set tokens to use as base token
		if tokenInfo.Symbol == "USDC" {
			abq.CommonPrices = append(abq.CommonPrices, tokenPrice{
				Symbol:  "WETH",
				Address: tokenInfo.Address,
				Price:   1,
			})
		}
		if tokenInfo.Symbol == "USDT" {
			abq.CommonPrices = append(abq.CommonPrices, tokenPrice{
				Symbol:  "USDT",
				Address: tokenInfo.Address,
				Price:   1,
			})
		}
		if tokenInfo.Symbol == "WETH" {
			price, err := GetPriceFromBinanceRetry("ETH")
			if err != nil {
				shared.Warnf(slog.Default(), "Setting CommonPrices - Failed to get WETH price, err: %v\n", err)
				continue
			}
			abq.CommonPrices = append(abq.CommonPrices, tokenPrice{
				Symbol:  "WETH",
				Address: tokenInfo.Address,
				Price:   price,
			})
		}

	}
	fmt.Printf("abq - got token infos\n")

	abq.UpdateTokenPrices()

	return abq
}

func (abq *AddressBalanceQuery) UpdateTokenPrices() {
	fmt.Printf("abq - updating common prices\n")

	// update CommonPrices
	wethIndex := -1
	wethAddress := shared.ZeroAddress
	for i, tokenPrice := range abq.CommonPrices {
		if tokenPrice.Symbol == "WETH" {
			wethIndex = i
			wethAddress = tokenPrice.Address
		}
	}

	if wethIndex >= 0 {
		abq.CommonPrices = RemoveTokenPrice(abq.CommonPrices, wethIndex)

		price, err := GetPriceFromBinanceRetry("ETH")
		if err != nil {
			shared.Warnf(slog.Default(), "Updating CommonPrices - Failed to get WETH price, err: %v\n", err)
		}
		abq.CommonPrices = append(abq.CommonPrices, tokenPrice{
			Symbol:  "WETH",
			Address: wethAddress,
			Price:   price,
		})
	}
	fmt.Printf("abq - updated common prices\n")

	fmt.Printf("abq - updating all token prices\n")
	// update prices of all other tokens
	abq.TokenInfosMutex.RLock()
	for tokenAddress, tokenInfo := range abq.TokenInfos {
		price, err := abq.GetTokenUsdPrice(tokenAddress)
		if err != nil {
			shared.Warnf(slog.Default(), "Failed to get price of %v: %v", tokenInfo.Symbol, err)
			continue
		}
		abq.TokenRatesMutex.Lock()
		abq.TokenRates[tokenInfo.Symbol] = price
		abq.TokenRatesMutex.Unlock()
		fmt.Printf("Price of %v: %v\n", tokenInfo.Symbol, price)
	}
	abq.TokenInfosMutex.RUnlock()
	fmt.Printf("abq - updated all token prices\n")
}

func RemoveTokenPrice(slice []tokenPrice, s int) []tokenPrice {
	return append(slice[:s], slice[s+1:]...)
}

func (abq *AddressBalanceQuery) GetTokenUsdPrice(tokenAddress common.Address) (float64, error) {
	tokenInfo := shared.RetrieveERC20Info(tokenAddress)
	tokenSymbol := tokenInfo.Symbol

	price, err := GetPriceFromBinanceRetry(tokenSymbol)
	if err == nil {
		fmt.Printf("binance - %v price: %v\n", tokenInfo.Symbol, price)
		return price, nil
	} else {
		// fmt.Printf("binance failed, using uniswap. err: %v\n", err)

		for _, commonPrice := range abq.CommonPrices {
			baseTokenAddress := commonPrice.Address
			baseTokenPrice := commonPrice.Price

			tokenPrice, err := GetPriceFromUniswap(abq.UniswapV3FactoryAddress, tokenAddress, baseTokenAddress)
			if err != nil {
				// fmt.Printf("err for baseTokenAddress %v, err %v\n", baseTokenAddress, err)
				continue
			}
			usdPrice := tokenPrice * baseTokenPrice
			// fmt.Printf("baseTokenAddress: %v\n", baseTokenAddress)
			// fmt.Printf("baseTokenPrice: %v\n", baseTokenPrice)
			// fmt.Printf("tokenPrice: %v\n", tokenPrice)
			fmt.Printf("uniswap - %v price: %v\n", tokenInfo.Symbol, usdPrice)
			return usdPrice, nil
		}
	}
	return 0, fmt.Errorf("unable to get price")
}

// TickerPriceResponse represents the structure of the response from the Binance API for the ticker price
type TickerPriceResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

// GetPriceFromBinance fetches the current market price for a given token symbol against USDT
func GetPriceFromBinance(symbol string) (float64, error) {
	// Construct the API URL
	url := fmt.Sprintf("https://api.binance.com/api/v3/ticker/price?symbol=%sUSDT", symbol)

	// Make the HTTP GET request
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	// Check the response status
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("error: received status code %d, body: %s", resp.StatusCode, body)
	}

	// Decode the JSON response
	var priceResp TickerPriceResponse
	if err := json.Unmarshal(body, &priceResp); err != nil {
		return 0, err
	}

	// Return the current market price
	price, err := strconv.ParseFloat(priceResp.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to convert %v to float", priceResp.Price)
	}
	return price, nil
}

// get price using Binance API, and will retry if got rate limited
func GetPriceFromBinanceRetry(symbol string) (float64, error) {
	for {
		price, err := GetPriceFromBinance(symbol)
		if err != nil {
			if strings.Contains(err.Error(), "Too much request weight used") {
				fmt.Printf("GetPriceFromBinanceRetry - Rate limited, retrying...")
				continue
			}
		}
		return price, err
	}
}

// tries to find uniswap V3 pool that swaps the specified tokens
// by querying GetPool, trying 4 different fee tiers
// returns price of token/baseToken
func GetPriceFromUniswap(uniswapV3FactoryAddress common.Address, tokenAddress common.Address, baseToken common.Address) (float64, error) {
	uniswapV3feeTiers := []int64{100, 500, 3000, 10000}

	caller, err := UniswapV3Factory.NewUniswapV3Factory(uniswapV3FactoryAddress, shared.Client)
	if err != nil {
		panic(err)
	}

	// token0 := common.HexToAddress("0xdAC17F958D2ee523a2206206994597C13D831ec7") // usdt
	token0 := baseToken
	token1 := tokenAddress

	blocknum := GetLatestBlockNum()

	callOptions := bind.CallOpts{
		BlockNumber: blocknum,
	}
	var poolAddress common.Address
	for _, fee := range uniswapV3feeTiers {
		pool, err := caller.GetPool(&callOptions, token0, token1, big.NewInt(fee))
		if err != nil {
			panic(err)
		}

		if pool.Cmp(shared.ZeroAddress) != 0 {
			// fmt.Printf("pool: %v\n", pool)
			poolAddress = pool
			break
		}
	}

	if poolAddress.Cmp(shared.ZeroAddress) == 0 {
		// log.Fatalf("unable to get pool with the tokens\n")
		return 0, fmt.Errorf("unable to get pool with the tokens\n")
	}

	opts := &bind.CallOpts{BlockNumber: blocknum}
	pool, err := uniswapPool.NewUniswapV3Pool(poolAddress, shared.Client)
	slot0, err := pool.Slot0(opts)
	priceWithDecimals := DecodeSqrtX96Price(slot0.SqrtPriceX96)

	// check token0 and token1 ordering
	swap := false
	if poolToken0, _ := pool.Token0(opts); poolToken0.Cmp(token0) == 0 {
		swap = false
	} else {
		// swap token0 and token1
		tmp := token0
		token0 = token1
		token1 = tmp

		swap = true
	}

	token0Info := shared.RetrieveERC20Info(token0)
	token1Info := shared.RetrieveERC20Info(token1)

	decimals0 := token0Info.Decimals
	decimals1 := token1Info.Decimals
	mult0 := big.NewFloat(0).SetInt(big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals0)), nil))
	mult1 := big.NewFloat(0).SetInt(big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals1)), nil))
	decimalsRatio := big.NewFloat(0).Quo(mult0, mult1)
	price := big.NewFloat(0).Mul(priceWithDecimals, decimalsRatio)

	// fmt.Printf("priceWithDecimals: %v\n", priceWithDecimals)
	// fmt.Printf("decimals0: %v\n", decimals0)
	// fmt.Printf("decimals1: %v\n", decimals1)
	// fmt.Printf("decimalsRatio: %v\n", decimalsRatio)
	// fmt.Printf("price: %v\n", price)

	priceFloat, _ := price.Float64()
	if !swap {
		priceFloat = 1 / priceFloat
	}
	return priceFloat, nil
}

// calculates balance of contract, in USD, balance = native ETH bal + token bal
func (a *AddressBalanceQuery) GetAddressUSDbalance(contract common.Address, blockNum *big.Int) float64 {
	var totalUSD float64

	prec := int64(6)

	// native ETH
	wei, _ := shared.Client.BalanceAt(context.Background(), contract, blockNum)
	divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(18-prec), nil)
	ether, _ := wei.Div(wei, divisor).Float64()
	ether = ether / math.Pow(10, float64(prec))

	a.TokenRatesMutex.RLock()
	totalUSD += ether * a.TokenRates["ETH"]
	a.TokenRatesMutex.RUnlock()
	if totalUSD != 0 {
		fmt.Printf("totalUSD: %v\n", totalUSD)
		fmt.Printf("ether: %v\n", ether)
	}

	// tokens
	a.TokenInfosMutex.Lock()
	defer a.TokenInfosMutex.Unlock()
	for tokenAddress, tokenInfo := range a.TokenInfos {
		balance, err := GetERC20BalanceOf(tokenAddress, contract, blockNum)
		if err != nil {
			log.Printf("AddressBalanceQuery - Error when getting erc20 balance of token %v! err %v\n", tokenAddress, err)
			continue
		}
		decimals := tokenInfo.Decimals
		symbol := tokenInfo.Symbol
		symbol = strings.ToUpper(symbol) // coin API uses uppercase symbols

		divisor := big.NewInt(0).Exp(big.NewInt(10), big.NewInt(int64(decimals)-prec), nil)
		amount, _ := wei.Div(balance, divisor).Float64()
		amount = amount / math.Pow(10, float64(prec))

		a.TokenRatesMutex.RLock()
		totalUSD += amount * a.TokenRates[symbol]
		a.TokenRatesMutex.RUnlock()
		if amount != 0 {
			// fmt.Printf("amount: %v\n", amount)
			// fmt.Printf("symbol: %v\n", symbol)
			// fmt.Printf("balance: %v\n", balance)
			// fmt.Printf("totalUSD: %v\n", totalUSD)
		}
	}

	return totalUSD
}
