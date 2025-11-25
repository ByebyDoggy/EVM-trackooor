package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	multicall2 "github.com/Zellic/EVM-trackooor/contracts/Multicall2"
	"github.com/Zellic/EVM-trackooor/shared"
	"github.com/Zellic/EVM-trackooor/utils"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/schollz/progressbar/v3"
)

var (
	initFuncSigsCalldata map[string][]byte
)

var initscanOptions struct {
	contractsFilepath string // path to json file, containing array of contracts (hex strings)
	// a limit on how many contracts to test init in total, for testing purposes.
	// if -1, no limit
	// if 0, continues straight onto realtime tracking
	contractsLimit    int
	fromAddress       common.Address   // address to use to call contracts
	checkAddresses    []common.Address // addresses to check in the state change. fromAddress will be automatically added to this.
	discordWebhookURL string
	discordWebhook    shared.WebhookInstance
	// psql
	psqlConnStr string
	// set to true to constantly go through psql database and init contracts, forever
	runForever bool
	// alert options
	USDthreshold float64
	// init after delay (seconds)
	initAfterDelay time.Duration
	// test init for contracts we know are initializable
	initKnownContractsFrequency time.Duration

	// wait for this duration before calling trace_block once a new block is mined
	// cus the fullnode might be slow on this
	// set this to like 2 seconds
	traceBlockDelay time.Duration

	// address of the multicall2 contract
	multicall2 common.Address
	// address of uniswapV3Factory on the chain
	uniswapV3Factory common.Address
}

// var initscanContracts []common.Address

// contracts that fulfilled the heuristics when some variation of init was called on it
// we wont store which variation of init since there could be multiple and its fine if we just spam all of them
// these contracts get repeatedly tested for init
type initializableContract struct {
	Contract common.Address `json:"contract"`
	Calldata []byte         `json:"calldata"`
}

var initializableContracts []initializableContract
var lastHistoricalInitAttemptTime time.Time
var initialisableContractsFilepath string

// for multicall2
// we will buffer up a lot of contracts before trying to init them all at once in 1 call
var contractsBuffer []common.Address
var contractsTraceBuffer []multicall2.Multicall2Call

// number of contracts contractsBuffer should hold before calling multicall
var contractsBufferLimit = 100

// if contractsTraceBuffer still hasnt hit the limit within this
// time, it will trace anyways
var lastTraceTime time.Time
var contractsTraceBufferTimeLimit = 1 * time.Minute

var trace_call_multicall2_WaitGroup sync.WaitGroup
var trace_call_multicall2_WaitGroupMutex sync.Mutex

type callInfo struct {
	ContainsFromAddr bool // state change contains from addr
	StateChange      bool
}

type flaggedContractInfo struct {
	SucceededFuncSigs map[string]callInfo
}

var flaggedContracts map[common.Address]flaggedContractInfo
var flaggedContractsMutex sync.RWMutex

var alreadyAlertedContracts []common.Address

var abq *utils.AddressBalanceQuery

// logging
var initscanLog *log.Logger

func (actionInfo) InfoInitscan() actionInfo {
	name := "Initscan"
	overview := "Finds deployed contracts that haven't been initialized yet."

	description := `There are two ways this module gets deployed contracts.
- Listens for blocks mined, and traces the block for any contract creation opcodes, similar to DeploymentScan.
  This is used for trying to init newly deployed contracts on a chain, in realtime.
- Given a PSQL connection string, it connects to the PSQL database and queries for contracts there.
  This is used for trying to init contracts already deployed, not contracts deployed from this point onwards.

The way contract initialization is tested is through the method described below:
- There are various init function signatures, which can be specified in the config.
- This may look like init(address) or init().
- For each init function signature, Initscan will first perform a normal eth_call to the contract with the function signature.
- If the call succeeds, Initscan will then trace_call the contract with the same func sig.
- Then, it will check the traces for state changes.
	- If the state change contains one of the "check-addresses" specified in the config, this is a good sign some state variable like owner changed
	- To reduce false positives due to contract fallback functions, another trace_call will be done with unrelated/random calldata.
		- If this trace_call reverts, or succeeds but does not contain one of the "check-addresses", then we will proceed to the last check. Otherwise, its a false positive and we ignore it.
- Finally, it will check the contract's funds, by aggregating its native ETH and ERC20 tokens specified in config.
- If the funds is above a certain USD threshold, it will alert via discord webhook.

Initscan will save contracts that are initializable, regardless of if the contract had funds. It will periodically try to init these known contracts, and alert accordingly if they now have funds.
`

	options := `"contracts-limit" - only used when getting contracts from PSQL database, upper limit for number of contracts to init. To init all contracts in PSQL database, set to -1, otherwise set to 0 to init contracts deployed from now on.
"initializable-contracts-filepath" - filepath to store contracts known to be initializable
"init-known-contracts-frequency" - (in seconds) frequency to try init contracts knwon to be initializable
"init-after-delay - (in seconds) duration to wait before trying to init newly deployed contract
"trace-block-delay" - (in seconds) duration to wait before tracing block (some RPC providers don't have trace block ready as soon as new block is mined)
"alert-usd-threshold" - (in USD) minimum USD requirement for contract to alert. set to 0 to alert regardless of funds
"multicall2" - address of multicall2 contract deployed on the chain
"uniswap-v3-factory" - address of UniswapV3Factory contract deployed on the chain (for USD price oracle)
"webhook-url" - discord webhook URL to send alerts to
"run-forever" - only for when initializing contracts in PSQL database, keep initializing contracts and don't run any other actions
"from-address" - address to send the eth_call and trace_call from
"check-addresses" - addresses to check for in the state changes of trace calls
"psql-connection-string" - PSQL database connection string, if initializing contracts in PSQL database
"function-signature-calldata" - map function signature to its calldata, these are the init func sigs and calldata that will be used
"tokens" - ERC20 tokens to be used in price oracle`

	example := `"Initscan": {
	"addresses": {},
	"options": {
		"contracts-limit": 0,
		"initializable-contracts-filepath":"./eth_initializable_contracts.json",
		"init-known-contracts-frequency":86400,
		"init-after-delay": 60,
		"trace-block-delay":5,
		"alert-usd-threshold": 1,
		"multicall2":"0x9695fa23b27022c7dd752b7d64bb5900677ecc21",
		"uniswap-v3-factory":"0x1F98431c8aD98523631AE4a59f267346ea31F984",
		"webhook-url": "https://discord.com/api/webhooks/...",
		"run-forever": false,
		"from-address": "0x4b20993bc481177ec7e8f571cecae8a9e22c02db",
		"check-addresses": [
			"0x4b20993bc481177ec7e8f571cecae8a9e22c02db",
			"0xca35b7d915458ef540ade6068dfe2f44e8fa733c",
			"0xd7acd2a9fd159e69bb102a1ca21c9a3e3a5f771b",
			"0xab8483f64d9c6d1ecf9b849ae677dd3315835cb2",
			"0x78731d3ca6b7e34ac0f824c42a7cc18a495cabab"
		],
		"psql-connection-string":"",
		"function-signature-calldata": {
			"initialize()": "0x8129fc1c",
			"init()": "0xe1c7392a",
			"init(address)": "0x19ab453c000000000000000000000000d7acd2a9fd159e69bb102a1ca21c9a3e3a5f771b",
			"initialize(uint256)": "0xfe4b84df0000000000000000000000000000000000000000000000487a9a304539440000"
		},
		"tokens": [
			"0xdac17f958d2ee523a2206206994597c13d831ec7",
			"0xB8c77482e45F1F44dE1745F52C74426C631bDD52",
			"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
			"0x628f76eab0c1298f7a24d337bbbf1ef8a1ea6a24",
			"0xae7ab96520de3a18e5e111b5eaab095312d7fe84"
		]
	}
}`

	return actionInfo{
		ActionName:          name,
		ActionOverview:      overview,
		ActionDescription:   description,
		ActionOptionDetails: options,
		ActionConfigExample: example,
	}
}

func (p action) InitInitscan() {
	// init maps
	flaggedContracts = make(map[common.Address]flaggedContractInfo)
	initFuncSigsCalldata = make(map[string][]byte)

	// logging
	initscanLog = shared.TimeLogger("[Initscan] ")

	// load config
	loadInitscanConfig(p.o.CustomOptions)

	var err error

	if v, ok := p.o.CustomOptions["initializable-contracts-filepath"]; ok {
		initialisableContractsFilepath = v.(string)
		f, _ := os.ReadFile(initialisableContractsFilepath)
		if len(f) != 0 {
			err := json.Unmarshal(f, &initializableContracts)
			if err != nil {
				initscanLog.Fatal(err)
			}
			initscanLog.Printf("Loaded %v initializable contracts from %v\n", len(initializableContracts), initialisableContractsFilepath)
		}
	} else {
		initscanLog.Fatalf("Please specify \"initializable-contracts-filepath\" !\n")
	}

	// address balance retriever to get USD value of address
	var tokens []common.Address
	for _, tokenAddrHex := range p.o.CustomOptions["tokens"].([]interface{}) {
		tokenAddress := common.HexToAddress(tokenAddrHex.(string))
		tokens = append(tokens, tokenAddress)
	}

	// abr = utils.NewAddressBalanceRetriever(initscanOptions.coinApiKey, tokens)
	abq = utils.NewAddressBalanceQuery(tokens, initscanOptions.uniswapV3Factory)

	// try init all contracts in the psql database

	// psql
	var connStr string
	if v, ok := p.o.CustomOptions["psql-connection-string"]; ok {
		connStr = v.(string)
	} else {
		// error if we're expecting to init contracts in database
		if initscanOptions.contractsLimit != 0 {
			log.Fatalf("Please specify \"psql-connection-string\"!")
		}
	}

	if connStr != "" {
		dbpool, err = pgxpool.New(context.Background(), connStr)
		if err != nil {
			log.Fatalf("Unable to connect to psql database, err: %v connStr: %v\n", err, connStr)
		}
	}

	initscanOptions.psqlConnStr = connStr

	// main init loop
	if initscanOptions.contractsLimit != 0 {
		initscanLog.Printf("Trying to init all contracts in database.")
		initscanLog.Printf("NOTE: other actions will be paused until this has finished")
		tryInitContractsInDatabase()
		if initscanOptions.runForever {
			for { // forever loop
				initscanLog.Printf("Forever loop - tryInitContractsInDatabase()")
				abq.UpdateTokenPrices()
				tryInitContractsInDatabase()
			}
		}
	}

	initscanLog.Printf("Tracking new blocks for deployments then trying to init them")
	addBlockAction(initscanHandleBlock)
}

func loadInitscanConfig(customOptions map[string]interface{}) {
	// if v, ok := customOptions["contracts-filepath"]; ok {
	// 	initscanOptions.contractsFilepath = v.(string)
	// } else {
	// 	initscanLog.Fatalf("Please provide \"contracts-filepath\"")
	// }
	// initscanLog.Printf("Using contracts filepath %v\n", initscanOptions.contractsFilepath)

	if v, ok := customOptions["contracts-limit"]; ok {
		initscanOptions.contractsLimit = int(v.(float64))
	}

	if v, ok := customOptions["from-address"]; ok {
		initscanOptions.fromAddress = common.HexToAddress(v.(string))
	} else {
		// random address
		initscanOptions.fromAddress = common.HexToAddress("0x5409eCb20EDcd221285E120ec5BCa309aa2FaB7A")
	}

	initscanOptions.checkAddresses = append(initscanOptions.checkAddresses, initscanOptions.fromAddress)
	if v, ok := customOptions["check-addresses"]; ok {
		addrHexInterfaces := v.([]interface{})
		for _, addrHexInterface := range addrHexInterfaces {
			addr := common.HexToAddress(addrHexInterface.(string))
			initscanOptions.checkAddresses = append(initscanOptions.checkAddresses, addr)
		}
	}

	if v, ok := customOptions["run-forever"]; ok {
		initscanOptions.runForever = v.(bool)
	} else {
		initscanOptions.runForever = false
	}

	if v, ok := customOptions["webhook-url"]; ok {
		webhookUrl := v.(string)
		initscanOptions.discordWebhookURL = webhookUrl
		initscanOptions.discordWebhook = shared.WebhookInstance{
			WebhookURL:           initscanOptions.discordWebhookURL,
			Username:             "initscan",
			Avatar_url:           "",
			RetrySendingMessages: true,
		}
		initscanLog.Printf("Using discord webhook url %v\n", initscanOptions.discordWebhookURL)
	} else {
		initscanLog.Printf("No discord webhook specified\n")
	}

	if v, ok := customOptions["function-signature-calldata"]; ok {
		initscanLog.Printf("Using func sigs:\n")
		functionSignatureCalldataMapInterface := v.(map[string]interface{})
		for funcSig, calldataHexInterface := range functionSignatureCalldataMapInterface {
			calldataHex := calldataHexInterface.(string)
			var calldata []byte
			if calldataHex[:2] == "0x" {
				calldata = common.Hex2Bytes(calldataHex[2:])
			} else {
				calldata = common.Hex2Bytes(calldataHex)
			}
			initFuncSigsCalldata[funcSig] = calldata
			initscanLog.Printf("%v\n", funcSig)
		}
	} else {
		initscanLog.Fatalf("Please specify \"function-signature-calldata\", mapping function signature to calldata in hex\n")
	}

	if initscanOptions.contractsLimit == -1 {
		initscanLog.Printf("No limit to contracts to init - will test init all contracts\n")
	} else {
		initscanLog.Printf("Limiting contracts amount to init to %v contracts\n", initscanOptions.contractsLimit)
	}
	if initscanOptions.runForever {
		initscanLog.Printf("Initscan will run forever! No other actions will run.\n")
	}

	if v, ok := customOptions["alert-usd-threshold"]; ok {
		initscanOptions.USDthreshold = v.(float64)
		initscanLog.Printf("Using $%v USD threshold for alerts\n", initscanOptions.USDthreshold)
	} else {
		initscanOptions.USDthreshold = 1 // default $1 USD
		initscanLog.Printf("Using default $%v USD threshold for alerts\n", initscanOptions.USDthreshold)
	}

	if v, ok := customOptions["init-after-delay"]; ok {
		initscanOptions.initAfterDelay = time.Second * time.Duration(v.(float64))
	} else {
		initscanOptions.initAfterDelay = time.Second * 1 // default 1 second
	}
	initscanLog.Printf("Initialising newly created contracts after %v delay\n", initscanOptions.initAfterDelay.String())

	if v, ok := customOptions["trace-block-delay"]; ok {
		initscanOptions.traceBlockDelay = time.Second * time.Duration(v.(float64))
	} else {
		initscanOptions.traceBlockDelay = time.Second * 2
	}

	if v, ok := customOptions["multicall2"]; ok {
		initscanOptions.multicall2 = common.HexToAddress(v.(string))
	} else {
		initscanLog.Fatalf("Please specify \"multicall2\" !")
	}

	if v, ok := customOptions["uniswap-v3-factory"]; ok {
		initscanOptions.uniswapV3Factory = common.HexToAddress(v.(string))
	} else {
		initscanLog.Fatalf("Please specify \"uniswap-v3-factory\" !")
	}

	if v, ok := customOptions["init-known-contracts-frequency"]; ok {
		initscanOptions.initKnownContractsFrequency = time.Second * time.Duration(v.(float64))
		initscanLog.Printf("Using initKnownContractsFrequency: %v\n", initscanOptions.initKnownContractsFrequency)
	} else {
		initscanOptions.initKnownContractsFrequency = time.Hour * 24
		initscanLog.Printf("Using default value for initKnownContractsFrequency: %v\n", initscanOptions.initKnownContractsFrequency)
	}

	initscanLog.Printf("Using from address %v\n", initscanOptions.fromAddress)
	initscanLog.Printf("Looking for these addresses in storage state changes: %v\n", initscanOptions.checkAddresses)
}

// func loadContractsFile() {
// 	initscanLog.Printf("Loading contract addresses from json filepath: %v\n", initscanOptions.contractsFilepath)

// 	file, err := os.Open(initscanOptions.contractsFilepath)
// 	if err != nil {
// 		panic(err)
// 	}
// 	defer file.Close()

// 	decoder := json.NewDecoder(file)

// 	// read start of array
// 	if _, err := decoder.Token(); err != nil {
// 		panic(err)
// 	}

// 	for decoder.More() {
// 		var contractAddr common.Address
// 		if err := decoder.Decode(&contractAddr); err != nil {
// 			initscanLog.Printf("Error decoding JSON: %v\n", err)
// 			return
// 		}
// 		initscanContracts = append(initscanContracts, contractAddr)
// 	}

// 	// read end of array
// 	if _, err := decoder.Token(); err != nil {
// 		panic(err)
// 	}

// 	initscanLog.Printf("Loaded %v contract addresses\n", len(initscanContracts))
// 	fmt.Printf("initscanContracts: %v\n", initscanContracts[0])
// }

func saveInitializableContracts() {
	initscanLog.Printf("Saved initializableContracts to %v\n", initialisableContractsFilepath)

	raw, err := json.Marshal(initializableContracts)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile(initialisableContractsFilepath, raw, 0644)
	if err != nil {
		panic(err)
	}
}

// use trace api to trace all txs in block
// to look for deployments
// fall back to debug api if trace api fails
func initscanHandleBlock(p ActionBlockData) {
	block := p.Block
	blockNum := block.NumberU64()
	blockNumBig := block.Number()

	// delay for historical
	if shared.Options.HistoricalOptions.FromBlock != nil && blockNum%10 == 0 {
		shared.BlockWaitGroup.Add(1)
		defer shared.BlockWaitGroup.Done()
	}

	// try init initialisable contracts again
	// we will do this every hour
	if initialisableContractsFilepath != "" && lastHistoricalInitAttemptTime.Add(initscanOptions.initKnownContractsFrequency).Before(time.Now()) {
		lastHistoricalInitAttemptTime = time.Now()
		go tryInitInitializableContractsAgain(blockNumBig)
	}

	// wait
	time.Sleep(initscanOptions.traceBlockDelay)

	// keep querying trace_replayBlockTransactions until it doesnt error
	// gives up after certain amount of attempts
	var blockTraces []shared.BlockTrace_Replay
	var err error
	var success bool
	for i := 0; i < 20; i++ {
		blockTraces, err = shared.Trace_replayBlockTransactions(blockNum, []string{"trace"})
		if err != nil {
			initscanLog.Printf("err: %v\n", err)
		} else {
			success = true
			break
		}
	}

	contractsCount := 0

	// Trace_replayBlockTransactions succeeds
	if success {
		for _, blockTrace := range blockTraces {
			for _, trace := range blockTrace.Traces {
				if trace.Type == "create" {
					contractCreated := trace.Result.Address
					// try init
					// go tryInitialiseContract(contractCreated, blockNumBig)

					// ignore if contract wasnt deployed properly
					// (e.g. out of gas, contract deployment reverted)
					if contractCreated.Cmp(shared.ZeroAddress) == 0 {
						continue
					}

					contractsBuffer = append(contractsBuffer, contractCreated)
					contractsCount += 1
				}
			}
		}

		// fmt.Printf("contractsBuffer: %v\n", contractsBuffer)
		// fmt.Printf("contractsCount: %v\n", contractsCount)
		if len(contractsBuffer) >= contractsBufferLimit {
			initscanLog.Printf("Done block %v, trying to init %v contracts\n", blockNum, len(contractsBuffer))
			tryInitializeContractsMulticall(initscanOptions.multicall2, contractsBuffer, blockNumBig)
		} else {
			initscanLog.Printf("Done block %v, added %v contracts to buffer (len %v)\n", blockNum, contractsCount, len(contractsBuffer))
		}

	} else {
		// Trace_replayBlockTransactions fails
		// fallback to debug_traceTransaction

		initscanLog.Printf("Unable to use trace_replayBlockTransactions for block %v\n", blockNum)
		initscanLog.Printf("Falling back to debug_traceTransaction\n")
		block, err := shared.SafeGetBlockByNumber(blockNumBig)
		if err != nil {
			initscanLog.Printf("SafeGetBlockByNumber err for block %v, skipping block\n", blockNum)
			return
		}
		for _, tx := range block.Transactions() {
			traceResult, err := shared.Debug_traceTransaction(tx.Hash())
			if err != nil {
				initscanLog.Printf("debug_traceTransaction skipping txhash: %v err: %v\n", tx.Hash(), err)
				continue
			}

			var from common.Address
			sender := utils.GetTxSender(tx)
			if sender != nil {
				from = *sender
			} else {
				from = shared.ZeroAddress
			}
			recursivelyTryInitDeployedContracts(traceResult, tx.Hash(), blockNumBig, from)
		}
		initscanLog.Printf("Done debug_traceTransaction for block %v\n", blockNum)
	}
}

func tryInitInitializableContractsAgain(blockNum *big.Int) {
	initscanLog.Printf("Trying to init %v initializable contracts\n", len(initializableContracts))
	lastHistoricalInitAttemptTime = time.Now()

	var newInitializableContracts []initializableContract

	for _, contractCalldata := range initializableContracts {
		contract := contractCalldata.Contract
		calldata := contractCalldata.Calldata
		success := tryInitialiseContractWithCalldata(contract, blockNum, calldata, funcSigGivenCallData(calldata), false)
		fmt.Printf("trying init %v with calldata %v\n", contract, calldata)
		if success {
			fmt.Printf("init success contract %v \n", contract)
			newInitializableContracts = append(newInitializableContracts, contractCalldata)
		} else {
			fmt.Printf("init FAIL contract %v \n", contract)
		}
	}

	initscanLog.Printf(
		"Removed %v contracts no longer initializable\n",
		len(initializableContracts)-len(newInitializableContracts),
	)

	initializableContracts = newInitializableContracts

	saveInitializableContracts()
}

// only called with data from debug_traceTransaction
// which is only used as a fallback when Trace_block fails
func recursivelyTryInitDeployedContracts(txTraceResult shared.TraceResult, txHash common.Hash, blockNum *big.Int, callerEOA common.Address, deployedContracts ...common.Address) []common.Address {
	switch txTraceResult.Type {
	case "CALL", "DELEGATECALL":
		// go through each internal call's trace result (if any) and recurse
		for _, tr := range txTraceResult.Calls {
			deployedContracts = recursivelyTryInitDeployedContracts(tr, txHash, blockNum, callerEOA, deployedContracts...)
		}
	case "CREATE", "CREATE2":
		deployerContract := txTraceResult.From
		contract := txTraceResult.To
		// bytecode := txTraceResult.Output

		initscanLog.Printf("recursive - try init contract: %v deployerContract: %v callerEOA: %v\n", contract, deployerContract, callerEOA)

		go tryInitialiseContract(contract, blockNum)

	default:
	}
	return deployedContracts
}

// check if tx was a deployment tx
// if it was, check if can initialise
func initscanCheckTxDeployment(tx *types.Transaction, blockNum *big.Int) {
	txType := shared.DetermineTxType(tx, blockNum)
	if txType != shared.DeploymentTx {
		return
	}

	contract, _ := shared.GetDeployedContractAddress(tx)
	tryInitialiseContract(contract, blockNum)
}

var contractWaitGroup sync.WaitGroup

func tryInitContractsInDatabase() {
	ctx := context.Background()

	// get amount of contracts in db
	var contractsAmount int
	err := dbpool.QueryRow(ctx, `select count(*) from contracts;`).Scan(&contractsAmount)
	if err != nil {
		initscanLog.Fatalf("Error getting amount of contracts: %v\n", err)
	}

	header, _ := shared.Client.HeaderByNumber(context.Background(), nil)
	latestBlockNum := header.Number

	// how many contracts to query from the db at once
	batchCount := 100000

	var upperLimit int
	if initscanOptions.contractsLimit == -1 {
		upperLimit = contractsAmount + batchCount
	} else {
		upperLimit = initscanOptions.contractsLimit
	}

	// progress bar
	progressBar := progressbar.Default(int64(upperLimit))
	initscanLog.Printf("Calling init on %v contracts\n", upperLimit)

	for offset := 0; offset <= upperLimit; offset += batchCount {
		// fetch contracts from db
		query := "SELECT address FROM CONTRACTS ORDER BY address LIMIT $1 OFFSET $2"
		rows, err := dbpool.Query(ctx, query, batchCount, offset)
		if err != nil {
			initscanLog.Fatalf("Err when querying, err: %v query: %v\n", err, query)
		}

		// go through all contracts

		var contracts []common.Address
		rowAmount := 0

		for rows.Next() {
			var contractHex string
			rows.Scan(&contractHex)
			contract := common.HexToAddress(contractHex)
			contracts = append(contracts, contract)

			rowAmount += 1
		}
		rows.Close()

		contractWaitGroup.Add(len(contracts))
		for _, contract := range contracts {
			go tryInitialiseContractAsync(contract, latestBlockNum)
		}

		// wait for currently trying to init contracts to finish
		contractWaitGroup.Wait()

		progressBar.Add(batchCount)
	}
	contractWaitGroup.Wait()

	// initscanLog.Printf("Finished calling init on all contracts, flagged %v contracts\n", len(flaggedContracts))
	initscanLog.Printf("Finished calling init on all contracts\n")
	// raw, _ := json.Marshal(flaggedContracts)
	// fmt.Printf("flaggedContracts: %v\n", string(raw)) // DEBUG
}

func tryInitializeContractsMulticall(multicall2Address common.Address, contracts []common.Address, blockNum *big.Int) {
	contractsBuffer = nil
	calls, results := multicallContractsWithInitFuncSigs(multicall2Address, contracts, blockNum)
	fmt.Printf("len contracts: %v len calls: %v len multicall results: %v\n", len(contracts), len(calls), len(results))

	numContractsAdded := 0

	for i, call := range calls {
		result := results[i]
		contract := call.Target
		calldata := call.CallData
		if result.Success {
			// ok instead of initialising it straight up we will add to another buffer
			// of contracts which we should trace_call
			// go tryInitialiseContractWithCalldata(contract, blockNum, calldata, funcSig)

			contractsTraceBuffer = append(contractsTraceBuffer, multicall2.Multicall2Call{
				Target:   contract,
				CallData: calldata,
			})

			numContractsAdded += 1
		}
	}

	initscanLog.Printf("tryInitialiseContractsMulticall: added %v to contractsTraceBuffer (len %v)\n", numContractsAdded, len(contractsTraceBuffer))

	now := time.Now()
	trace_call_multicall2_WaitGroup.Wait()
	if len(contractsTraceBuffer) > contractsBufferLimit ||
		lastTraceTime.Add(contractsTraceBufferTimeLimit).Before(now) {

		initscanLog.Printf("calling trace_call_multicall2 on all contractsTraceBuffer (%v) blocknum: %v\n", len(contractsTraceBuffer), blockNum)

		trace_call_multicall2_WaitGroupMutex.Lock()
		trace_call_multicall2_WaitGroup.Add(1)
		trace_call_multicall2_WaitGroupMutex.Unlock()

		trace_call_multicall2(initscanOptions.multicall2, contractsTraceBuffer, blockNum)

		trace_call_multicall2_WaitGroupMutex.Lock()
		trace_call_multicall2_WaitGroup.Done()
		trace_call_multicall2_WaitGroupMutex.Unlock()

		initscanLog.Printf("done call trace_call_multicall2 blocknum: %v\n", blockNum)

		lastTraceTime = now
	}

}

func funcSigGivenCallData(calldata []byte) string {
	for funcSig, _calldata := range initFuncSigsCalldata {
		if bytes.Equal(calldata, _calldata) {
			return funcSig
		}
	}
	return ""
}

func trace_call_multicall2(multicall2Address common.Address, calls []multicall2.Multicall2Call, blockNum *big.Int) {

	contractsTraceBuffer = nil

	if len(calls) == 0 {
		initscanLog.Printf("calls is empty. blocknum %v\n", blockNum)
		return
	}

	instance, err := multicall2.NewMulticall2(multicall2Address, shared.Client)
	if err != nil {
		initscanLog.Fatalf("Failed to make multicall2 instance err: %v\n", err)
	}

	calldata, err := instance.Encode_Multicall2Call(false, calls)
	if err != nil {
		initscanLog.Fatalf("Encode_Multicall2Call failed, err: %v\n", err)
	}

	traceCallResult, err := shared.Trace_call(shared.CallParams{
		From:  initscanOptions.fromAddress,
		To:    multicall2Address,
		Value: (*hexutil.Big)(big.NewInt(0)),
		Data:  calldata,
	}, []string{"trace", "stateDiff"})
	if err != nil {

		initscanLog.Printf("\n\ntrace_call_multicall2 - Trace_call err: %v \n\n", err)

		// too many, lets split them up
		if len(calls) > 10 {
			initscanLog.Printf("splitting up for blocknum %v\n", blockNum)
			// split calls into subslices of size 10
			size := 10
			var subCallSlices [][]multicall2.Multicall2Call
			var j int
			for i := 0; i < len(calls); i += size {
				j += size
				if j > len(calls) {
					j = len(calls)
				}
				subCallSlices = append(subCallSlices, calls[i:j])
			}

			for _, subCallSlice := range subCallSlices {
				trace_call_multicall2(multicall2Address, subCallSlice, blockNum)
			}
			initscanLog.Printf("done all split calls for blocknum %v\n", blockNum)
		} else {
			// now we will trace call each of the addresses
			// one by one
			initscanLog.Printf("doing linear for blocknum %v\n", blockNum)
			for _, call := range calls {
				contract := call.Target
				calldata := call.CallData
				go traceCallAndRecord(contract, calldata, blockNum, funcSigGivenCallData(calldata), true)
			}
			initscanLog.Printf("DONE linear for blocknum %v\n", blockNum)
		}

		// initscanLog.Fatalf("Trace_call err: %v\n", err)
		return
	}

	// fmt.Printf("trace_call_multicall2 - len traces: %v\n", len(traceCallResult.Trace))
	// fmt.Printf("statediff: %+v\n", traceCallResult.StateDiff)

	// we wont even look at traces
	// we will just look at the state diff
	// and see if any of it contains a check address

	containsCheckAddr := false
	for addr, entry := range traceCallResult.StateDiff {
		// ignore entry if its the fromAddress (if tx succeeds, from address nonce will increase, which is recorded)
		if addr.Cmp(initscanOptions.fromAddress) == 0 {
			// fmt.Printf("ignoring from addr with entry.Storage: %v\n", string(entry.Storage))
			continue
		}

		if string(entry.Storage) != "{}" {
			for _, checkAddr := range initscanOptions.checkAddresses {
				if strings.Contains(
					strings.ToLower(string(entry.Storage)),
					strings.ToLower(checkAddr.Hex()[2:]),
				) {
					containsCheckAddr = true
				}
			}
		}
	}

	if containsCheckAddr {
		initscanLog.Printf("blockNum: %v , CONTAINS CHECK ADDR! time to do one by one for %v calls %v initFuncSigsCalldata\n", blockNum, len(calls), len(initFuncSigsCalldata))

		// too many, lets split them up
		if len(calls) > 10 {
			initscanLog.Printf("splitting up for blocknum %v\n", blockNum)
			// split calls into subslices of size 10
			size := 10
			var subCallSlices [][]multicall2.Multicall2Call
			var j int
			for i := 0; i < len(calls); i += size {
				j += size
				if j > len(calls) {
					j = len(calls)
				}
				subCallSlices = append(subCallSlices, calls[i:j])
			}

			for _, subCallSlice := range subCallSlices {
				trace_call_multicall2(multicall2Address, subCallSlice, blockNum)
			}
			initscanLog.Printf("done all split calls for blocknum %v\n", blockNum)
		} else {
			// now we will trace call each of the addresses
			// one by one
			initscanLog.Printf("doing linear for blocknum %v\n", blockNum)
			for _, call := range calls {
				contract := call.Target
				calldata := call.CallData
				go traceCallAndRecord(contract, calldata, blockNum, funcSigGivenCallData(calldata), true)
			}
			initscanLog.Printf("DONE linear for blocknum %v\n", blockNum)
		}
	}

	// for _, trace := range traceCallResult.Trace {
	// 	fmt.Printf("trace: %+v\n", trace)
	// 	if trace.Error == "" {
	// 		fmt.Printf("no error?\n")
	// 	}
	// }

}

func multicallContractsWithInitFuncSigs(multicall2Address common.Address, contracts []common.Address, blockNum *big.Int) ([]multicall2.Multicall2Call, []multicall2.Multicall2Result) {
	calls := []multicall2.Multicall2Call{}
	for _, contract := range contracts {
		for _, calldata := range initFuncSigsCalldata {
			call := multicall2.Multicall2Call{
				Target:   contract,
				CallData: calldata,
			}
			calls = append(calls, call)
		}
	}

	return multicallContracts(multicall2Address, calls, blockNum)
}

func multicallContracts(multicall2Address common.Address, calls []multicall2.Multicall2Call, blockNum *big.Int) ([]multicall2.Multicall2Call, []multicall2.Multicall2Result) {
	fmt.Printf("multicallContracts called with len calls %v\n", len(calls))

	instance, err := multicall2.NewMulticall2(multicall2Address, shared.Client)
	if err != nil {
		initscanLog.Fatalf("Failed to make multicall2 instance err: %v\n", err)
	}
	callOptions := bind.CallOpts{
		From:        initscanOptions.fromAddress,
		BlockNumber: blockNum,
	}

	results, err := instance.TryAggregate_static(&callOptions, false, calls)

	if err != nil {
		if len(calls) > 1 {
			// initscanLog.Printf("multicallContracts err, will split up into smaller chunks. multicall2Address: %v blockNum: %v len contracts: %v err : %v\n", multicall2Address, blockNum, len(contracts), err)
			initscanLog.Printf("multicallContracts err, will split up into smaller chunks. multicall2Address: %v blockNum: %v len calls: %v err: %v\n", multicall2Address, blockNum, len(calls), err)
			// we might have queried with too many contracts, lets do it again but half the contracts
			// split contract into sub-slices
			size := len(calls) / 10
			if size <= 0 {
				size = 1
			}
			var subCallSlices [][]multicall2.Multicall2Call
			var j int
			for i := 0; i < len(calls); i += size {
				j += size
				if j > len(calls) {
					j = len(calls)
				}
				subCallSlices = append(subCallSlices, calls[i:j])
			}

			var calls []multicall2.Multicall2Call
			var results []multicall2.Multicall2Result

			for _, subCallSlice := range subCallSlices {
				fmt.Printf("len sub contract slice %v\n", len(subCallSlice))

				_calls, res := multicallContracts(multicall2Address, subCallSlice, blockNum)
				calls = append(calls, _calls...)
				results = append(results, res...)
			}
			initscanLog.Printf("splitted - calls: %v results: %v\n", len(calls), len(results))
			return calls, results
		} else {
			initscanLog.Fatalf("multicallContracts err, len contracts is already 1.")
		}
	}

	initscanLog.Printf("multicallContracts success - calls: %v results: %v\n", len(calls), len(results))
	return calls, results
}

func tryInitialiseContractAsync(contractAddress common.Address, blockNum *big.Int) {
	tryInitialiseContract(contractAddress, blockNum)
	contractWaitGroup.Done()
}

// tries to init contract
// returns true if some variation of init worked, and there was state change
// containing some critical address
// it loops through all initFuncSigsCalldata
func tryInitialiseContract(contractAddress common.Address, blockNum *big.Int) bool {
	// wait for `delay` seconds, then after that try init the contract
	time.Sleep(initscanOptions.initAfterDelay)

	could := false
	for funcSig, calldata := range initFuncSigsCalldata {
		a, b, _ := traceCallAndRecord(contractAddress, calldata, blockNum, funcSig, true)
		if a && b {
			could = true
		}
	}
	return could
}

// try to init contract with specific calldata
func tryInitialiseContractWithCalldata(contractAddress common.Address, blockNum *big.Int, calldata []byte, funcSig string, waitDelay bool) bool {
	if waitDelay {
		// wait for `delay` seconds, then after that try init the contract
		time.Sleep(initscanOptions.initAfterDelay)
	}

	a, b, _ := traceCallAndRecord(contractAddress, calldata, blockNum, funcSig, true)
	return a && b
}

// uses trace_call to call a contract with calldata and see if storage changes.
// returns a tuple (bool, bool, bool)
// which is:
// whether or not state changed
// whether or not state change contained the fromAddr
// whether or not contract has funds (contract funds > 0)
// if call did not succeed, all will be false regardless
// parameter `record` is whether or not to record this in the psql database
func traceCallAndRecord(contractAddress common.Address, calldata []byte, blockNum *big.Int, funcSig string, record bool) (bool, bool, bool) {
	if contractAddress.Cmp(shared.ZeroAddress) == 0 {
		return false, false, false
	}

	// DEBUG
	// initscanLog.Printf("traceCallAndRecord called for %v\n", contractAddress)

	var traceCallResult shared.TraceCallResult
	var err error
	for trial := 0; trial < 10; trial++ {
		traceCallResult, err = shared.Trace_call(shared.CallParams{
			From:  initscanOptions.fromAddress,
			To:    contractAddress,
			Value: (*hexutil.Big)(big.NewInt(0)),
			Data:  calldata,
		}, []string{"trace", "stateDiff"})
		if err != nil {
			// initscanLog.Fatalf("Trace_call err: %v\n", err)
			initscanLog.Printf("Trace_call err, will retry (trial %v): %v\n", trial, err)
			continue
		}
		break
	}

	// initscanLog.Printf("done trace call for %v\n", contractAddress)

	// check if call succeeded
	successful := true
	for _, trace := range traceCallResult.Trace {
		if trace.Error != "" {
			successful = false
			// fmt.Printf("%v trace.Error: %v funcsig %v\n", contractAddress, trace.Error, funcSig)
		}
	}
	if successful {
		// check if state changed
		stateChanged := false
		containsCheckAddr := false
		for addr, entry := range traceCallResult.StateDiff {
			// ignore entry if its the fromAddress (if tx succeeds, from address nonce will increase, which is recorded)
			if addr.Cmp(initscanOptions.fromAddress) == 0 {
				// fmt.Printf("ignoring from addr with entry.Storage: %v\n", string(entry.Storage))
				continue
			}

			if string(entry.Storage) != "{}" {
				stateChanged = true
				for _, checkAddr := range initscanOptions.checkAddresses {
					if strings.Contains(
						strings.ToLower(string(entry.Storage)),
						strings.ToLower(checkAddr.Hex()[2:]),
					) {
						containsCheckAddr = true
						// fmt.Printf("CONTAINS CHECK ADDR check addr: %v addr: %v storage: %v\n", checkAddr, addr, string(entry.Storage))
					}
				}
				// fmt.Printf("state changed, addr: %v storage: %v\n", addr, string(entry.Storage))
			}
		}

		// only record if state changed
		if stateChanged {
			// fmt.Printf("state changed. %v containsCheckAddr: %v\n", contractAddress, containsCheckAddr)

			// flaggedContractsMutex.Lock()
			// // init inner map if not already init
			// if flaggedContracts[contractAddress].SucceededFuncSigs == nil {
			// 	info := flaggedContractInfo{
			// 		SucceededFuncSigs: make(map[string]callInfo),
			// 	}
			// 	flaggedContracts[contractAddress] = info
			// }

			// // record info
			// info := flaggedContracts[contractAddress].SucceededFuncSigs
			// info[funcSig] = callInfo{
			// 	ContainsFromAddr: containsFromAddr,
			// 	StateChange:      stateChanged,
			// }
			// flaggedContractsMutex.Unlock()

			stateChangeRaw, err := json.Marshal(traceCallResult.StateDiff)
			if err != nil {
				initscanLog.Fatalf("error when marshalling traceCallResult.StateDiff err: %v\n", err)
			}
			// contractBal := abq.GetAddressUSDbalance(contractAddress, blockNum)
			contractBal := abq.GetAddressUSDbalance(contractAddress, utils.GetLatestBlockNum())
			surpassesUSDthreshold := contractBal >= float64(initscanOptions.USDthreshold)
			// record info in psql database
			if record {
				// record only if psql database connstr provided
				if initscanOptions.psqlConnStr != "" {
					go recordFlaggedContractPSQL(contractAddress, funcSig, stateChangeRaw, containsCheckAddr, contractBal)
				}

				// check if storage change containing fromAddr was proper, by calling with some other calldata
				random_calldata := common.Hex2Bytes("6fcb831b")
				_, b2, _ := traceCallAndRecord(contractAddress, random_calldata, blockNum, "_stakeLp()", false)
				if containsCheckAddr && !b2 { // not a fluke

					fmt.Printf("not fluke!!? contract: %v contractBal: %v\n", contractAddress, contractBal)

					alreadyContains := false
					for _, v := range initializableContracts {
						if v.Contract.Cmp(contractAddress) == 0 {
							alreadyContains = true
						}
					}
					if !alreadyContains {
						fmt.Printf("added %v to initializableContracts\n", contractAddress)
						initializableContracts = append(initializableContracts, initializableContract{
							Contract: contractAddress,
							Calldata: calldata,
						}) // its initialisable!
						saveInitializableContracts()
					}

					if surpassesUSDthreshold {
						if !slices.Contains(alreadyAlertedContracts, contractAddress) { // avoid duplicate alerts
							// alert on discord webhook (if specified)
							initscanLog.Printf("Interesting contract found\n")
							initscanLog.Printf("contract: %v\n", contractAddress)
							initscanLog.Printf("contractBal: %v\n", contractBal)
							initscanLog.Printf("contractBal: %v\n", funcSig)
							alreadyAlertedContracts = append(alreadyAlertedContracts, contractAddress)
							if initscanOptions.discordWebhookURL != "" {
								initscanLog.Printf("Sending webhook msg")

								if shared.ChainID == big.NewInt(1) {
									initscanOptions.discordWebhook.SendMessage(fmt.Sprintf(
										"# Interesting contract\n"+
											"Address: %v\n"+
											"Funds (USD): $%v\n"+
											"funcSig: %v\n",
										utils.FormatBlockscanHyperlink("address", utils.ShortenAddress(contractAddress), contractAddress.Hex()),
										contractBal,
										funcSig,
									))
								} else {
									chainIdName := shared.ChainIdName[shared.ChainID.String()]

									initscanOptions.discordWebhook.SendMessage(fmt.Sprintf(
										"# Interesting contract on chain %v (%v)\n"+
											"Address: %v\n"+
											"Funds (USD): $%v\n"+
											"funcSig: %v\n",
										shared.ChainID,
										chainIdName,
										utils.FormatBlockscanHyperlink("address", utils.ShortenAddress(contractAddress), contractAddress.Hex()),
										contractBal,
										funcSig,
									))
								}

							}
						}
					}
				}
			}
			return true, containsCheckAddr, surpassesUSDthreshold
		}
		return false, false, false
	}
	return false, false, false
}

func recordFlaggedContractPSQL(contractAddress common.Address, funcSig string, stateChange []byte, containsFromAddr bool, contractUSDValue float64) {
	// debugCount += 1
	/*
		table made by
		CREATE TABLE flagged_contracts (
		address VARCHAR(100) NOT NULL,
		succeeded_func_sig VARCHAR(200) NOT NULL,
		state_change BYTEA,
		contains_from_address BOOLEAN NOT NULL,
		contract_balance FLOAT4,
		PRIMARY KEY (address, succeeded_func_sig)
		);
	*/

	ctx := context.Background()

	tx, err := dbpool.Begin(ctx)
	if err != nil {
		initscanLog.Fatalf("dbpool.Begin err: %v\n", err)
	}
	defer tx.Rollback(ctx)

	query := `
	INSERT INTO flagged_contracts (address, succeeded_func_sig, state_change, contains_from_address, contract_balance)
	VALUES ($1, $2, $3, $4, $5)
	ON CONFLICT (address, succeeded_func_sig) DO UPDATE
	SET
	state_change=EXCLUDED.state_change,
	contains_from_address=EXCLUDED.contains_from_address,
	contract_balance=EXCLUDED.contract_balance
	`
	_, err = tx.Exec(ctx, query, contractAddress.Hex(), funcSig, stateChange, containsFromAddr, contractUSDValue)
	if err != nil {
		initscanLog.Fatal("Error inserting into contracts table:", err)
	}

	// commit changes
	tx.Commit(ctx)
}

func (p action) FinishedInitscan() {
	// DEBUG
	// for historical testing
	initscanLog.Printf("Waiting for finish...\n")
	initscanLog.Printf("will wait for 2 mins then wait for waitgroup...\n")

	time.Sleep(10 * time.Second)

	go tryInitializeContractsMulticall(initscanOptions.multicall2, contractsBuffer, shared.Options.HistoricalOptions.ToBlock)

	time.Sleep(2 * time.Minute)

	initscanLog.Printf("Done!\n")
}
