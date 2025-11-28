package shared

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

type nodeHealth struct {
	failTimes []time.Time
	disabled  bool
}

type WrappedClient struct {
	nodes   []string
	clients []*ethclient.Client
	mu      sync.Mutex
	rand    *rand.Rand
	health  map[int]*nodeHealth
}

var WrappedClientInstance *WrappedClient

const (
	failThreshold = 2               // 5 次失败
	failWindow    = 5 * time.Minute // 5 分钟
)

// 初始化客户端
func NewWrappedClient(nodeURLs []string) {
	clients := make([]*ethclient.Client, len(nodeURLs))
	health := make(map[int]*nodeHealth)
	for i, url := range nodeURLs {
		c, err := ethclient.Dial(url)
		if err != nil {
			log.Fatalf("Failed to connect to node %s: %v", url, err)
		}
		clients[i] = c
		health[i] = &nodeHealth{failTimes: []time.Time{}, disabled: false}
	}

	WrappedClientInstance = &WrappedClient{
		nodes:   nodeURLs,
		clients: clients,
		rand:    rand.New(rand.NewSource(time.Now().UnixNano())),
		health:  health,
	}
	go func() {
		for {
			UpdateNodes()
			time.Sleep(30 * time.Minute)
		}
	}()
}

// 随机选择一个健康节点客户端
func GetRandomClient() (client *ethclient.Client, idx int) {
	WrappedClientInstance.mu.Lock()
	defer WrappedClientInstance.mu.Unlock()

	// 收集所有可用节点
	available := []int{}
	now := time.Now()
	for i, h := range WrappedClientInstance.health {
		// 清理过期失败记录
		newFailTimes := []time.Time{}
		for _, t := range h.failTimes {
			if now.Sub(t) <= failWindow {
				newFailTimes = append(newFailTimes, t)
			}
		}
		h.failTimes = newFailTimes

		// 判断是否禁用
		if h.disabled {
			continue
		}
		if len(h.failTimes) >= failThreshold {
			h.disabled = true
			//log.Printf("[NodeHealth] Node #%d (%s) disabled due to %d failures in last %v", i, WrappedClientInstance.nodes[i], len(h.failTimes), failWindow)
			continue
		}
		available = append(available, i)
	}

	if len(available) == 0 {
		//log.Println("[NodeHealth] Warning: all nodes disabled, fallback to full list")
		for i := range WrappedClientInstance.clients {
			available = append(available, i)
			WrappedClientInstance.health[i].disabled = false // 重置
		}
	}

	// 随机选择
	idx = available[WrappedClientInstance.rand.Intn(len(available))]
	client = WrappedClientInstance.clients[idx]
	return
}

// FilterLogs 带节点切换 + 日志打印 + 节点健康管理
func FilterLogs(ctx context.Context, query ethereum.FilterQuery) (logs []types.Log, err error) {
	maxRetries := len(WrappedClientInstance.clients)
	tried := make(map[int]bool)

	for attempt := 0; attempt < maxRetries; attempt++ {
		client, idx := GetRandomClient()
		if tried[idx] {
			continue
		}
		tried[idx] = true

		//log.Printf("[FilterLogs] Using node #%d (%s)", idx, WrappedClientInstance.nodes[idx])

		logs, err = client.FilterLogs(ctx, query)
		if err == nil {
			//log.Printf("[FilterLogs] Success: found %d logs", len(logs))
			return logs, nil
		}

		//log.Printf("[FilterLogs] Error from node #%d: %v", idx, err)
		markNodeFail(idx)
		time.Sleep(500 * time.Millisecond) // 避免瞬间重复请求
	}

	return nil, fmt.Errorf("all nodes failed, last error: %v", err)
}

// 记录节点失败
func markNodeFail(idx int) {
	WrappedClientInstance.mu.Lock()
	defer WrappedClientInstance.mu.Unlock()

	h := WrappedClientInstance.health[idx]
	h.failTimes = append(h.failTimes, time.Now())
	if len(h.failTimes) >= failThreshold {
		h.disabled = true
		//log.Printf("[NodeHealth] Node #%d (%s) disabled due to %d failures in last %v", idx, WrappedClientInstance.nodes[idx], len(h.failTimes), failWindow)
	}
}
