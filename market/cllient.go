package market

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ethereum/go-ethereum/common"
)

var BaseCoinMarketURL string

// TokenInfo represents basic token information
type TokenInfo struct {
	Address  common.Address `json:"address"`
	Name     string         `json:"name"`
	Symbol   string         `json:"symbol"`
	Decimals int            `json:"decimals"`
}

// ExchangeSpot represents an exchange spot
type ExchangeSpot struct {
	ExchangeName string `json:"exchange_name"`
	SpotName     string `json:"spot_name"`
}

// ExchangeContract represents an exchange contract
type ExchangeContract struct {
	ExchangeName string `json:"exchange_name"`
	ContractName string `json:"contract_name"`
}

// OnChainInfo represents on-chain information
type OnChainInfo struct {
	ChainName       string `json:"chain_name"`
	ContractAddress string `json:"contract_address"`
}

// SupplyInfo represents supply information
type SupplyInfo struct {
	TotalSupply       *float64 `json:"total_supply,omitempty"`
	CirculatingSupply *float64 `json:"circulating_supply,omitempty"`
	CachedPrice       *float64 `json:"cached_price,omitempty"`
	MarketCap         *float64 `json:"market_cap,omitempty"`
}

// CoinInfo represents comprehensive coin information
type CoinInfo struct {
	CoinID            string             `json:"coin_id"`
	Symbol            string             `json:"symbol"`
	Name              string             `json:"name"`
	ExchangeSpots     []ExchangeSpot     `json:"exchange_spots"`
	ExchangeContracts []ExchangeContract `json:"exchange_contracts"`
	OnChainInfo       []OnChainInfo      `json:"on_chain_info"`
	SupplyInfo        *SupplyInfo        `json:"supply_info,omitempty"`
}

// SearchRequest represents a search request
type SearchRequest struct {
	SearchTerm string `json:"search_term"`
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status          string `json:"status"`
	Initialized     bool   `json:"initialized"`
	CoinCount       int    `json:"coin_count"`
	SearchIndexSize int    `json:"search_index_size"`
}

// RootResponse represents the root endpoint response
type RootResponse struct {
	Message     string `json:"message"`
	Status      string `json:"status"`
	Initialized bool   `json:"initialized"`
}

// GetTokenInfo fetches token information
func (t *TokenInfo) GetTokenInfo() (*TokenInfo, error) {
	// Implementation would go here
	return t, nil
}

// SearchCoins searches for coins based on a search term (POST version)
func SearchCoins(searchTerm string) ([]CoinInfo, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Create the search request
	searchReq := SearchRequest{
		SearchTerm: searchTerm,
	}

	// Marshal the request to JSON
	reqBody, err := json.Marshal(searchReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal search request: %w", err)
	}

	// Create HTTP request
	clientURL := BaseCoinMarketURL + "/search"
	req, err := http.NewRequest("POST", clientURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Perform the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var coins []CoinInfo
	if err := json.NewDecoder(resp.Body).Decode(&coins); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return coins, nil
}

// SearchCoinsGet searches for coins based on a search term (GET version)
func SearchCoinsGet(searchTerm string) ([]CoinInfo, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Encode the search term
	encodedTerm := url.QueryEscape(searchTerm)

	// Create HTTP request
	clientURL := fmt.Sprintf("%s/search?search_term=%s", BaseCoinMarketURL, encodedTerm)
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var coins []CoinInfo
	if err := json.NewDecoder(resp.Body).Decode(&coins); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return coins, nil
}

// GetCoinByID fetches detailed information for a specific coin by ID
func GetCoinByID(coinID string) (*CoinInfo, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Create HTTP request
	clientURL := fmt.Sprintf("%s/coin/%s", BaseCoinMarketURL, coinID)
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var coinInfo CoinInfo
	if err := json.NewDecoder(resp.Body).Decode(&coinInfo); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &coinInfo, nil
}

// GetCoinByContract fetches coin information by contract address
func GetCoinByContract(contractAddress string) ([]CoinInfo, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Create HTTP request
	clientURL := fmt.Sprintf("%s/coin/by_contract/%s", BaseCoinMarketURL, contractAddress)
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var coins []CoinInfo
	if err := json.NewDecoder(resp.Body).Decode(&coins); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return coins, nil
}

// GetAllCoins fetches all coins with pagination
func GetAllCoins(limit int) ([]CoinInfo, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Ensure limit is within bounds
	if limit < 1 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}

	// Create HTTP request
	clientURL := fmt.Sprintf("%s/coins?limit=%d", BaseCoinMarketURL, limit)
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var coins []CoinInfo
	if err := json.NewDecoder(resp.Body).Decode(&coins); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return coins, nil
}

// HealthCheck performs a health check on the service
func HealthCheck() (*HealthResponse, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Create HTTP request
	clientURL := BaseCoinMarketURL + "/health"
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var health HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &health, nil
}

// RootEndpoint fetches the root endpoint information
func RootEndpoint() (*RootResponse, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}

	// Create HTTP request
	clientURL := BaseCoinMarketURL
	resp, err := http.Get(clientURL)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// Decode the response
	var root RootResponse
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &root, nil
}
