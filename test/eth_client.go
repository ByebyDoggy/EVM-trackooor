package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// 自定义结构体，适配 Arbitrum 区块格式（包含特有字段）
type ArbitrumBlock struct {
	Hash         common.Hash           `json:"hash"`
	Transactions []ArbitrumTransaction `json:"transactions"`
	UncleHashes  []common.Hash         `json:"uncles"`
	Withdrawals  []*types.Withdrawal   `json:"withdrawals,omitempty"`
	// 其他需要的字段可按需添加（如 gasLimit、timestamp 等）
}
type txExtraInfo struct {
	BlockNumber *string         `json:"blockNumber,omitempty"`
	BlockHash   *common.Hash    `json:"blockHash,omitempty"`
	From        *common.Address `json:"from,omitempty"`
}

// 自定义交易结构体，支持 Arbitrum 特殊交易类型
type ArbitrumTransaction struct {
	tx *types.Transaction
	// tx *Transaction
	txExtraInfo
	// 其他需要的字段（如 gas、input、value 等）
}

// hacky code for L2 blocks

type rpcBlock struct {
	Hash         common.Hash         `json:"hash"`
	Transactions []rpcTransaction    `json:"transactions"`
	UncleHashes  []common.Hash       `json:"uncles"`
	Withdrawals  []*types.Withdrawal `json:"withdrawals,omitempty"`
}

type rpcTransaction struct {
	tx *types.Transaction
	// tx *Transaction
	txExtraInfo
}

// Transaction types.
const (
	LegacyTxType     = 0x00
	AccessListTxType = 0x01
	DynamicFeeTxType = 0x02
	BlobTxType       = 0x03
)

// Transaction is an Ethereum transaction.
type Transaction struct {
	inner TxData    // Consensus contents of a transaction
	time  time.Time // Time first seen locally (spam avoidance)

	// caches
	hash atomic.Pointer[common.Hash]
	size atomic.Uint64
	from atomic.Pointer[sigCache]
}

// sigCache is used to cache the derived sender and contains
// the signer used to derive it.
type sigCache struct {
	signer Signer
	from   common.Address
}

// Signer encapsulates transaction signature handling. The name of this type is slightly
// misleading because Signers don't actually sign, they're just for validating and
// processing of signatures.
//
// Note that this interface is not a stable API and may change at any time to accommodate
// new protocol rules.
type Signer interface {
	// Sender returns the sender address of the transaction.
	Sender(tx *Transaction) (common.Address, error)

	// SignatureValues returns the raw R, S, V values corresponding to the
	// given signature.
	SignatureValues(tx *Transaction, sig []byte) (r, s, v *big.Int, err error)
	ChainID() *big.Int

	// Hash returns 'signature hash', i.e. the transaction hash that is signed by the
	// private key. This hash does not uniquely identify the transaction.
	Hash(tx *Transaction) common.Hash

	// Equal returns true if the given signer is the same as the receiver.
	Equal(Signer) bool
}

// AccessList is an EIP-2930 access list.
type AccessList []AccessTuple

// AccessTuple is the element type of an access list.
type AccessTuple struct {
	Address     common.Address `json:"address"     gencodec:"required"`
	StorageKeys []common.Hash  `json:"storageKeys" gencodec:"required"`
}

type TxData interface {
	txType() byte // returns the type ID
	copy() TxData // creates a deep copy and initializes all fields

	chainID() *big.Int
	accessList() AccessList
	data() []byte
	gas() uint64
	gasPrice() *big.Int
	gasTipCap() *big.Int
	gasFeeCap() *big.Int
	value() *big.Int
	nonce() uint64
	to() *common.Address

	rawSignatureValues() (v, r, s *big.Int)
	setSignatureValues(chainID, v, r, s *big.Int)

	// effectiveGasPrice computes the gas price paid by the transaction, given
	// the inclusion block baseFee.
	//
	// Unlike other TxData methods, the returned *big.Int should be an independent
	// copy of the computed value, i.e. callers are allowed to mutate the result.
	// Method implementations can use 'dst' to store the result.
	effectiveGasPrice(dst *big.Int, baseFee *big.Int) *big.Int

	encode(*bytes.Buffer) error
	decode([]byte) error
}

func (tx *rpcTransaction) UnmarshalJSON(msg []byte) error {
	if err := json.Unmarshal(msg, &tx.tx); err != nil {
		return err
	}
	return json.Unmarshal(msg, &tx.txExtraInfo)
}

// get a block by number hex string, or `latest` / `pending` / `earliest`
func GetL2BlockByHexNumber(number *big.Int, client *ethclient.Client) (*types.Block, error) {
	var raw json.RawMessage
	if err := client.Client().CallContext(context.Background(), &raw, "eth_getBlockByNumber", hexutil.EncodeBig(number), true); err != nil {
		return nil, fmt.Errorf("rpc call failed: %w", err)
	}
	var head types.Header
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("failed to unmarshal header: %w", err)
	}

	// 用原始 JSON 接收交易
	var rawBody struct {
		Hash         common.Hash         `json:"hash"`
		Transactions []json.RawMessage   `json:"transactions"`
		UncleHashes  []common.Hash       `json:"uncles"`
		Withdrawals  []*types.Withdrawal `json:"withdrawals,omitempty"`
	}

	if err := json.Unmarshal(raw, &rawBody); err != nil {
		return nil, fmt.Errorf("failed to unmarshal block body: %w", err)
	}

	txs := make([]*types.Transaction, 0, len(rawBody.Transactions))
	for _, rawTx := range rawBody.Transactions {
		var txObj types.Transaction
		if err := json.Unmarshal(rawTx, &txObj); err != nil {
			log.Printf("跳过无法解析交易: %v", err)
			continue
		}

		if txObj.Type() <= BlobTxType { // 仅保留标准交易类型 0-3
			txs = append(txs, &txObj)
		}
		//else {
		//	log.Printf("跳过非标准交易: %s, 类型: %d", txObj.Hash().Hex(), txObj.Type())
		//}
	}

	return types.NewBlockWithHeader(&head).WithBody(types.Body{
		Transactions: txs,
		Withdrawals:  rawBody.Withdrawals,
	}), nil
}

// 获取区块的同时，获取该区块内的所有日志
func GetL2BlockWithLogs(blockNumber *big.Int, client *ethclient.Client) (*types.Block, []types.Log, error) {
	// 1. 获取区块基本信息
	block, err := GetL2BlockByHexNumber(blockNumber, client)
	if err != nil {
		return nil, nil, fmt.Errorf("获取区块失败: %w", err)
	}

	// 2. 构建日志过滤条件：查询该区块内的所有日志
	query := ethereum.FilterQuery{
		FromBlock: blockNumber, // 起始区块（目标区块）
		ToBlock:   blockNumber, // 结束区块（目标区块，与起始相同表示只查该区块）
		// Addresses: []common.Address{}, // 可选：指定合约地址，不填则查所有合约的日志
	}

	// 3. 调用 eth_getLogs 获取该区块的所有日志
	logs, err := client.FilterLogs(context.Background(), query)
	if err != nil {
		return nil, nil, fmt.Errorf("获取区块日志失败: %w", err)
	}

	return block, logs, nil
}

func main() {
	// 连接 Arbitrum 节点（注意：查询日志建议用 HTTPS 端点，WSS 也支持但更适合订阅）
	client, err := ethclient.Dial("https://bnb-mainnet.g.alchemy.com/v2/4VBDnuze-voa9UtA1mOe7SZl_1ii4y3D")
	if err != nil {
		log.Fatalf("无法连接到节点: %v", err)
	}
	defer client.Close()

	// 要查询的区块高度
	blockHeight := big.NewInt(69124876) // 替换为目标区块号

	// 获取区块及其中的所有日志
	block, logs, err := GetL2BlockWithLogs(blockHeight, client)
	if err != nil {
		fmt.Println("无法获取区块或日志:", err)
		return
	}

	// 打印区块基本信息
	fmt.Printf("区块高度: %d\n", block.NumberU64())
	fmt.Printf("区块哈希: %s\n", block.Hash().Hex())
	fmt.Println("交易数量:", len(block.Transactions()))

	//for _, tx := range block.Transactions() {
	//	fmt.Println(tx.Hash().Hex())
	//	fmt.Println(tx.Nonce())
	//	fmt.Println(hexutil.Encode(tx.Data()))
	//}

	// 打印区块中的日志
	fmt.Printf("\n区块内的日志数量: %d\n", len(logs))
	for i, log := range logs {
		fmt.Printf("\n日志 %d:\n", i+1)
		fmt.Printf("  触发日志的合约地址: %s\n", log.Address.Hex())
		fmt.Printf("  交易哈希: %s\n", log.TxHash.Hex())
		fmt.Printf("  日志索引: %d\n", log.Index)
		fmt.Printf("  Topics (事件签名+索引参数): %v\n", log.Topics)
		fmt.Printf("  Data (非索引参数): 0x%x\n", log.Data)
	}
}
