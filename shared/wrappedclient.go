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
	failThreshold    = 2                      // 2次失败禁用节点
	failWindow       = 5 * time.Minute        // 5分钟失败窗口
	singleReqTimeout = 10 * time.Second       // 单次节点请求超时时间
	retryBackoffBase = 500 * time.Millisecond // 基础重试间隔
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

	// 新增：后台协程定期重置禁用节点（避免永久禁用）
	go resetDisabledNodes()
}

// 定期重置超过failWindow的禁用节点
func resetDisabledNodes() {
	ticker := time.NewTicker(failWindow)
	defer ticker.Stop()

	for range ticker.C {
		WrappedClientInstance.mu.Lock()
		now := time.Now()
		for _, h := range WrappedClientInstance.health {
			if !h.disabled {
				continue
			}
			// 清理过期失败记录
			newFailTimes := []time.Time{}
			for _, t := range h.failTimes {
				if now.Sub(t) <= failWindow {
					newFailTimes = append(newFailTimes, t)
				}
			}
			h.failTimes = newFailTimes
			// 失败次数低于阈值则恢复节点
			if len(h.failTimes) < failThreshold {
				h.disabled = false
				//log.Printf("[NodeHealth] Node #%d (%s) re-enabled after fail window", idx, WrappedClientInstance.nodes[idx])
			}
		}
		WrappedClientInstance.mu.Unlock()
	}
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

// 自定义超时错误类型
var ErrFilterLogsTimeout = fmt.Errorf("filter logs request timeout")

// FilterLogs 带节点切换 + 日志打印 + 节点健康管理 + 超时控制
// 新增超时保护，避免goroutine阻塞，优化重试逻辑
func FilterLogs(ctx context.Context, query ethereum.FilterQuery) (logs []types.Log, err error) {
	maxRetries := len(WrappedClientInstance.clients)
	if maxRetries == 0 {
		return nil, fmt.Errorf("no available eth clients")
	}

	tried := make(map[int]bool)
	lastErr := error(nil)

	// 1. 为整体请求添加超时控制（总超时 = 节点数 × 单次超时 + 缓冲时间）
	totalTimeout := time.Duration(maxRetries)*singleReqTimeout + 2*time.Second
	ctx, cancel := context.WithTimeout(ctx, totalTimeout)
	defer cancel() // 确保上下文最终关闭，释放资源

	for attempt := 0; attempt < maxRetries; attempt++ {
		// 检查上下文是否已超时/取消，提前退出
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrFilterLogsTimeout, ctx.Err())
		default:
		}

		client, idx := GetRandomClient()
		if tried[idx] {
			continue
		}
		tried[idx] = true

		//log.Printf("[FilterLogs] Using node #%d (%s)", idx, WrappedClientInstance.nodes[idx])

		// 2. 为单次节点请求添加独立超时，避免单个节点卡死
		reqCtx, reqCancel := context.WithTimeout(ctx, singleReqTimeout)
		logs, err = client.FilterLogs(reqCtx, query)
		reqCancel() // 立即释放单次请求上下文，避免泄露

		if err == nil {
			//log.Printf("[FilterLogs] Success: found %d logs", len(logs))
			return logs, nil
		}

		// 记录最后一次错误
		lastErr = err
		//log.Printf("[FilterLogs] Error from node #%d: %v", idx, err)

		markNodeFail(idx)

		// 3. 退避策略：重试间隔随失败次数递增，避免高频重试
		backoff := retryBackoffBase * time.Duration(attempt+1)
		time.Sleep(backoff)
	}

	return nil, fmt.Errorf("all %d nodes failed, last error: %v", maxRetries, lastErr)
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
