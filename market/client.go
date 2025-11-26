package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/machinebox/graphql"
)

var BaseCoinMarketURL string
var graphqlClient *graphql.Client
var clientOnce sync.Once

func initializeGraphQLClient() {
	clientOnce.Do(func() {
		// 如果BaseCoinMarketURL已设置且不为空，则使用它
		baseURL := BaseCoinMarketURL
		if baseURL == "" {
			// 否则使用默认URL
			baseURL = "http://127.0.0.1:8000"
		}

		// 构建完整的GraphQL URL
		graphqlURL := baseURL
		if !strings.HasSuffix(graphqlURL, "/graphql") && !strings.HasSuffix(graphqlURL, "/graphql/") {
			if graphqlURL[len(graphqlURL)-1] != '/' {
				graphqlURL += "/graphql"
			} else {
				graphqlURL += "graphql"
			}
		}

		// 确保URL以http://或https://开头
		if !strings.HasPrefix(graphqlURL, "http://") && !strings.HasPrefix(graphqlURL, "https://") {
			graphqlURL = "http://" + graphqlURL
		}

		graphqlClient = graphql.NewClient(graphqlURL)
	})
}

// HealthResponse represents the health check response
type HealthResponse struct {
	Status          string `json:"status"`
	Initialized     bool   `json:"initialized"`
	CoinCount       int    `json:"coin_count"`
	SearchIndexSize int    `json:"search_index_size"`
}

// GraphQL响应结构体 - 根据实际响应格式修正
type GraphQLCoinResponse struct {
	CoinsByContractAddress []struct {
		CurrentPrice *float64 `json:"currentPrice"`
		MarketCap    *float64 `json:"marketCap"`
		MaxSupply    *float64 `json:"maxSupply"`
	} `json:"coinsByContractAddress"`
}

// GetTokenPriceByContract fetches token price by contract address
func GetTokenPriceByContract(contractAddress string) (float64, error) {
	// 检查GraphQL客户端是否正确初始化
	initializeGraphQLClient()
	if graphqlClient == nil {
		return 0, fmt.Errorf("failed to initialize GraphQL client")
	}

	// 创建GraphQL查询
	query := `
		query GetCoinsByContractAddress($contractAddress: String!) {
			coinsByContractAddress(contractAddress: $contractAddress) {
				currentPrice
				marketCap
				maxSupply
			}
		}
	`
	// 创建请求对象
	req := graphql.NewRequest(query)
	req.Var("contractAddress", contractAddress)

	// 设置上下文和超时
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// 执行查询
	var resp GraphQLCoinResponse
	if err := graphqlClient.Run(ctx, req, &resp); err != nil {
		return 0, fmt.Errorf("failed to execute GraphQL query: %w", err)
	}

	// 提取价格信息
	if len(resp.CoinsByContractAddress) == 0 {
		return 0, fmt.Errorf("no price data found for contract address: %s", contractAddress)
	}

	// 返回第一个有效的价格
	for _, coin := range resp.CoinsByContractAddress {
		if coin.CurrentPrice != nil {
			return *coin.CurrentPrice, nil
		}
	}

	return 0, fmt.Errorf("no valid price found for contract address: %s", contractAddress)
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
