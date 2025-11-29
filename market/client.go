package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	Price struct {
		CurrentPrice float64 `json:"price"`
	} `json:"price"`
}

type GraphQLTopProjectsResponse struct {
	ContractExchanges []struct {
		Coin struct {
			ContractAddress string `json:"contractAddress"`
			ChainName       string `json:"chainName"`
		}
		CurrentPrice *float64 `json:"currentPrice"`
		Holdings     []struct {
			Balance *float64 `json:"balance"`
			Holder  struct {
				Address   string `json:"address"`
				ChainType string `json:"chainType"`
			} `json:"holder"`
		} `json:"holdings"`
	} `json:"contractExchanges"`
}

type GraphQLTokenResponse struct {
	HolderDetail struct {
		Coins []struct {
			CoinId string `json:"coinId"`
		} `json:"coins"`
	} `json:"holderDetail"`
}

type GraphQLMonitTokenResponse struct {
	ContractExchanges []struct {
		Coin struct {
			OnChainInfos []struct {
				ChainName       string `json:"chainName"`
				ContractAddress string `json:"contractAddress"`
			} `json:"onChainInfos"`
			Name string `json:"name"`
		}
	} `json:"contractExchanges"`
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
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Printf("failed to close response body: %v", err)
		}
	}(resp.Body)

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

// GetTokenPriceByContract fetches token price by contract address
func GetTokenPriceByContract(contractAddress string) (float64, error) {
	// 检查GraphQL客户端是否正确初始化
	initializeGraphQLClient()
	if graphqlClient == nil {
		return 0, fmt.Errorf("failed to initialize GraphQL client")
	}

	// 创建GraphQL查询
	query := `
	query MyQuery ($contractAddress: String!) {
  price(contractAddress: $contractAddress) {
    price
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
	if resp.Price.CurrentPrice <= 0 {
		return 0, fmt.Errorf("no valid price found for contract address: %s", contractAddress)
	}

	return resp.Price.CurrentPrice, nil
}

type GraphQLTopProjectsDetailedResponse struct {
	ContractExchanges []struct {
		Coin struct {
			CurrentPrice *float64 `json:"currentPrice"`
			Holdings     []struct {
				Balance *float64 `json:"balance"`
				Holder  struct {
					Address   string `json:"address"`
					ChainType string `json:"chainType"`
				} `json:"holder"`
			} `json:"holdings"`
		} `json:"coin"`
	} `json:"contractExchanges"`
}

// 修改fetchTopProjectsAddressByChainID函数，只负责与GraphQL交互
func fetchTopProjectsAddressByChainID(chainID string, limit int, offset int) (*GraphQLTopProjectsDetailedResponse, bool, error) {
	initializeGraphQLClient()
	if graphqlClient == nil {
		return nil, false, fmt.Errorf("failed to initialize GraphQL client")
	}

	// 创建GraphQL查询 - 修复变量声明问题
	query := `
		query fetchTopProjectsAddressByChainID($limit: Int!, $offset: Int!) {
			contractExchanges(exchangeId: "binance_futures", limit: $limit, offset: $offset) {
				coin {
					currentPrice
					holdings {
						balance
						holder {
							address
							chainType
						}
					}
				}
			}
		}
	`

	// 创建请求对象
	req := graphql.NewRequest(query)
	req.Var("limit", limit)
	req.Var("offset", offset)

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// 执行查询
	var resp GraphQLTopProjectsDetailedResponse
	if err := graphqlClient.Run(ctx, req, &resp); err != nil {
		return nil, false, fmt.Errorf("failed to execute GraphQL query: %w", err)
	}

	isEnd := len(resp.ContractExchanges) < limit
	return &resp, isEnd, nil
}
func fetchCoinIdByProjectAddress(projectAddress string) (string, error) {
	initializeGraphQLClient()
	if graphqlClient == nil {
		return "", fmt.Errorf("failed to initialize GraphQL client")
	}
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 创建GraphQL查询
	query := `
		query MyQuery ($projectAddress: String!) {
  holderDetail(holderAddress: $projectAddress) {
    coins {
      coinId
    }
  }
}
	`
	// 创建请求对象
	req := graphql.NewRequest(query)
	req.Var("projectAddress", projectAddress)

	// 执行查询
	var resp GraphQLTokenResponse
	if err := graphqlClient.Run(ctx, req, &resp); err != nil {
		return "", fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	// 提取地址信息
	if len(resp.HolderDetail.Coins) == 0 {
		return "", fmt.Errorf("no address found for project address when invoke FetchCoinIdByProjectAddress: %s", projectAddress)
	}
	if len(resp.HolderDetail.Coins) == 0 {
		return "", fmt.Errorf("no coin found for project address when invoke FetchCoinIdByProjectAddress: %s", projectAddress)
	}
	return resp.HolderDetail.Coins[0].CoinId, nil
}

func fetchMonitTokenAddressesByChainID(chainID string, limit int, offset int) ([]string, bool, error) {
	initializeGraphQLClient()
	if graphqlClient == nil {
		return nil, false, fmt.Errorf("failed to initialize GraphQL client")
	}
	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	query := `
	query FetchMonitTokenAddressesByChainID ($limit: Int!, $offset: Int!) {
  contractExchanges(exchangeId: "binance_futures", limit: $limit, offset: $offset) {
    coin {
      onChainInfos {
        chainName
        contractAddress
      }
      name
    }
  }
}`
	req := graphql.NewRequest(query)
	req.Var("limit", limit)
	req.Var("offset", offset)
	// 执行查询
	var resp GraphQLMonitTokenResponse
	if err := graphqlClient.Run(ctx, req, &resp); err != nil {
		return nil, false, fmt.Errorf("failed to execute GraphQL query: %w", err)
	}
	isEnd := len(resp.ContractExchanges) < limit
	// 提取地址信息
	if len(resp.ContractExchanges) == 0 {
		return nil, isEnd, nil
	}
	var resultAddresses []string
	for _, coin := range resp.ContractExchanges {
		for _, onChainInfo := range coin.Coin.OnChainInfos {
			if onChainInfo.ChainName == chainID {
				resultAddresses = append(resultAddresses, onChainInfo.ContractAddress)
			}
		}
	}
	if len(resultAddresses) == 0 {
		return nil, isEnd, fmt.Errorf("no price data found for chain address: %s", chainID)
	} else {
		return resultAddresses, isEnd, nil
	}
}

type HTTPFetchPriceByContractResponse struct {
	ContractAddresses string  `json:"contract_address"`
	CoinId            string  `json:"coin_id"`
	Price             float64 `json:"price"`
}

type HTTPFetchContractAddressByExchangeTypeResponseItem struct {
	ContractAddress string `json:"contract_address"`
	ChainName       string `json:"chain_name"`
}

// 通过HTTP请求获取代币价格
func fetchTokenPriceByContractHTTP(contractAddress string) (string, float64, error) {
	if BaseCoinMarketURL == "" {
		return "", 0, fmt.Errorf("BaseCoinMarketURL not set")
	}
	quickSearchPrefix := BaseCoinMarketURL + "/quick-search"
	// 构建请求URL
	url := fmt.Sprintf("%s/price/%s", quickSearchPrefix, contractAddress)

	// 创建HTTP请求
	resp, err := http.Get(url)
	if err != nil {
		return "", 0, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", 0, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var response HTTPFetchPriceByContractResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return "", 0, fmt.Errorf("failed to decode response: %w", err)
	}

	// 返回价格
	if response.Price <= 0 {
		return "", 0, fmt.Errorf("invalid price received: %f", response.Price)
	}

	return response.CoinId, response.Price, nil
}

// 通过HTTP请求获取指定交易所的代币地址列表
func fetchMonitTokenAddressesByExchangeIDHTTP(exchangeID string, chain_name string) ([]string, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}
	quickSearchPrefix := BaseCoinMarketURL + "/quick-search"
	// 构建请求URL
	url := fmt.Sprintf("%s/exchange/%s", quickSearchPrefix, exchangeID)

	// 创建HTTP请求
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}
	var contractList []HTTPFetchContractAddressByExchangeTypeResponseItem
	// 解析响应
	if err := json.NewDecoder(resp.Body).Decode(&contractList); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// 提取合约地址
	var resultAddresses []string
	for _, contract := range contractList {
		if contract.ChainName == chain_name {
			resultAddresses = append(resultAddresses, contract.ContractAddress)
		}
	}

	return resultAddresses, nil
}

// 通过HTTP请求获取指定链的顶级持有者地址
func fetchTopHoldersAddressByChainIDHTTP(chainID string) ([]string, error) {
	if BaseCoinMarketURL == "" {
		return nil, fmt.Errorf("BaseCoinMarketURL not set")
	}
	quickSearchPrefix := BaseCoinMarketURL + "/quick-search"
	// 构建请求URL
	url := fmt.Sprintf("%s/top-holders/%s", quickSearchPrefix, chainID)

	// 创建HTTP请求
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to perform HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP request failed with status: %d, body: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var addresses []string
	if err := json.NewDecoder(resp.Body).Decode(&addresses); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return addresses, nil
}
