package shared

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
)

type chainlistRPC struct {
	URL      string `json:"url"`
	Tracking string `json:"tracking"`
}

type chainlistItem struct {
	Name  string         `json:"name"`
	Chain string         `json:"chain"`
	RPC   []chainlistRPC `json:"rpc"`
}

// UpdateNodes 从 Chainlist 获取最新 RPC 节点，加入可用列表
func UpdateNodes() {
	wc := WrappedClientInstance
	resp, err := http.Get("https://chainlist.org/rpcs.json")
	if err != nil {
		log.Printf("[UpdateNodes] Failed to fetch Chainlist: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[UpdateNodes] Read body error: %v", err)
		return
	}

	var chains []chainlistItem
	if err := json.Unmarshal(body, &chains); err != nil {
		log.Printf("[UpdateNodes] JSON unmarshal error: %v", err)
		return
	}

	wc.mu.Lock()
	defer wc.mu.Unlock()

	added := 0
	for _, chain := range chains {
		if chain.Chain != Options.UpdateNodesChainName {
			continue
		}
		for _, rpc := range chain.RPC {
			url := strings.TrimSpace(rpc.URL)
			if !strings.HasPrefix(url, "http") {
				continue // 只要 http/https
			}
			if rpc.Tracking == "limited" {
				continue
			}

			// 排除黑名单节点（failTimes过多或禁用节点）
			skip := false
			for i, nodeURL := range wc.nodes {
				if nodeURL == url {
					skip = true
					break
				}
				if wc.health[i].disabled && len(wc.health[i].failTimes) >= failThreshold {
					skip = true
					break
				}
			}
			if skip {
				continue
			}

			// 创建新客户端并加入列表
			c, err := ethclient.Dial(url)
			if err != nil {
				log.Printf("[UpdateNodes] Failed to connect to %s: %v", url, err)
				continue
			}

			wc.nodes = append(wc.nodes, url)
			wc.clients = append(wc.clients, c)
			wc.health[len(wc.clients)-1] = &nodeHealth{failTimes: []time.Time{}, disabled: false}
			added++
		}
	}

	log.Printf("[UpdateNodes] Added %d new RPC nodes, total nodes: %d", added, len(wc.nodes))
}
