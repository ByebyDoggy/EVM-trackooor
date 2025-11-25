package actions

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/Zellic/EVM-trackooor/utils"

	"github.com/Zellic/EVM-trackooor/shared"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5/pgxpool"
)

// psql
var dbpool *pgxpool.Pool

var psqlWaitGroup WaitGroupCount

// max amount of concurrent functions running to record contracts in psql
var psqlWaitGroupMaximum = int64(5000)
var psqlIsWaiting = false

var psqlDeadlockRetryTimes = 20

type contractTableData struct {
	Address          common.Address
	Bytecode         []byte
	DeployerContract common.Address
	DeployerEOA      common.Address
	BlockNum         *big.Int
}

type contractSelfdestructData struct {
	Address              common.Address
	SelfDestructBlocknum *big.Int
}

// logging
var deploymentScanLog *log.Logger

// var blockProcessedMutex sync.WaitGroup

func (p actionInfo) InfoDeploymentScan() actionInfo {
	name := "DeploymentScan"
	overview := "Retrieves all contracts deployed in realtime, or in a given block range (historical), " +
		"including those deployed through internal transactions. MUST be ran with flag --batch-fetch-blocks"

	description := `Deployed contracts are recorded in a PostgreSQL database, with the following tables:
	
CREATE DATABASE deployment_scan;

CREATE TABLE contracts (
	address VARCHAR(100) PRIMARY KEY,
	bytecode_hash VARCHAR(100),
	blocknum BIGINT,
	self_destruct_blocknum BIGINT,
	deployer_eoa_address VARCHAR(100),
	deployer_contract_address VARCHAR(100)
);

CREATE TABLE bytecodes (
	bytecode_hash VARCHAR(100) PRIMARY KEY,
	bytecode BYTEA
);

CREATE TABLE block_timestamps (
	blocknum BIGINT PRIMARY KEY,
	timestamp TIMESTAMP NOT NULL
);

It uses three tables
- one with contracts mapping to the contract's bytecode hash
- another mapping bytecode hash to bytecode, as a lot of contracts have the same source code, so we can reduce storage and filter unique bytecodes this way
- block_timestamps is for mapping a block number to timestamp, this is purely for generating graphs of the data vs time

It retrieves all contracts by using Trace API call trace_block to trace all txs in a block, and falls back to Debug API debug_traceTransaction, debug tracing each tx in a block, if Trace API fails.
For reference, tracing all blocks on Ethereum mainnet from genesis took ~3 days with a local Erigon full node.`

	options := `"psql-connection-string" - PostgreSQL connection string`

	example := `"DeploymentScan": {
    "addresses": {},
    "options":{
        "psql-connection-string":"postgresql://[user[:password]@][netloc][:port]"
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

// this action gets a list of all deployed contracts (deployed within a given timeframe)
func (p action) InitDeploymentScan() {
	// init logging
	deploymentScanLog = shared.TimeLogger("[Deployment scan] ")

	/*
		psql tables were created like this

		CREATE DATABASE deployment_scan;

		CREATE TABLE contracts (
		address VARCHAR(100) PRIMARY KEY,
		bytecode_hash VARCHAR(100) NOT NULL
		);

		CREATE TABLE bytecodes (
		bytecode_hash VARCHAR(100) PRIMARY KEY,
		bytecode BYTEA NOT NULL
		);

		update: adding blocknum and timestamp contract was deployed (so we can make cool graphs)

		ALTER TABLE contracts
		ADD COLUMN blocknum BIGINT;

		CREATE TABLE block_timestamps (
			blocknum BIGINT PRIMARY KEY,
			timestamp TIMESTAMP NOT NULL
		);

		update #2: adding contract deployer addresses, total gas spent by EOAs

		ALTER TABLE contracts
		ADD COLUMN self_destruct_blocknum BIGINT,
		ADD COLUMN deployer_eoa_address VARCHAR(100),
		ADD COLUMN deployer_contract_address VARCHAR(100);

		dropping NOT NULL constraint on bytecode
		(if we run it not from genesis block, and contract selfdestructs,
		if the contract is not indexed, we cannot do UPDATE, thus
		we must insert a row in with just contract address and selfdestruct
		number)

		ALTER TABLE contracts
		ALTER bytecode_hash DROP NOT NULL;

	*/

	var connStr string
	if v, ok := p.o.CustomOptions["psql-connection-string"]; ok {
		connStr = v.(string)
	} else {
		log.Fatalf("Please specify \"psql-connection-string\"!")
	}
	var err error
	dbpool, err = pgxpool.New(context.Background(), connStr)
	if err != nil {
		log.Fatalf("Unable to connect to psql database, err: %v connStr: %v\n", err, connStr)
	}

	// listen to blocks mined
	addBlockAction(deploymentScanHandleBlock)
}

func deploymentScanHandleBlock(p ActionBlockData) {
	block := p.Block
	blockNum := block.Number()

	// first we will record the timestamp of the block
	timestamp := time.Unix(int64(block.Time()), 0)
	go recordBlockTimestamp(blockNum, timestamp)

	// then we trace block transactions to record deployed contracts
	TraceBlock(blockNum.Uint64())
}

// using trace API

func TraceBlock(blockNum uint64) {
	if shared.Options.HistoricalOptions.FromBlock != nil {
		defer shared.BlockWaitGroup.Done()
	}

	// keep querying trace_replayBlockTransactions until it doesnt error
	// gives up after certain amount of attempts
	var blockTraces []shared.BlockTrace
	var err error
	var success bool
	var didFail bool
	for i := 0; i < 20; i++ {
		blockTraces, err = shared.Trace_block(blockNum)
		if err != nil {
			deploymentScanLog.Printf("errored but will retry, err: %v\n", err)
			time.Sleep(time.Duration(blockNum%10) * time.Minute)
			didFail = true
		} else {
			success = true
			if didFail {
				fmt.Printf("block %v suceeded now\n", blockNum)
			}
			break
		}
	}

	if success {
		var callerEOA common.Address
		contractDatas, contractSelfdestructDatas := processBlockTrace(blockTraces, callerEOA, blockNum)
		if len(contractDatas) > 0 {
			fmt.Printf("blocknum %v batch recording %v contractData\n", blockNum, len(contractDatas))
			go batchRecordContractToPSQL(contractDatas)
			// fmt.Printf("blocknum %v finished\n", blockNum)
		}

		if len(contractSelfdestructDatas) > 0 {
			fmt.Printf("blocknum %v batch recording %v contractSelfdestructDatas\n", blockNum, len(contractSelfdestructDatas))
			go batchRecordSelfDestruct(contractSelfdestructDatas)
		}

	} else {
		deploymentScanLog.Printf("Unable to use Trace_block for block %v\n", blockNum)
		deploymentScanLog.Printf("Falling back to debug_traceTransaction\n")
		block, err := shared.SafeGetBlockByNumber(big.NewInt(int64(blockNum)))
		if err != nil {
			deploymentScanLog.Printf("SafeGetBlockByNumber err for block %v, skipping block\n", blockNum)
			return
		}
		for _, tx := range block.Transactions() {
			traceResult, err := shared.Debug_traceTransaction(tx.Hash())
			if err != nil {
				deploymentScanLog.Printf("debug_traceTransaction skipping txhash: %v err: %v\n", tx.Hash(), err)
				continue
			}

			if traceResult.Error != "" {
				continue
			}

			var from common.Address
			sender := utils.GetTxSender(tx)
			if sender != nil {
				from = *sender
			} else {
				from = shared.ZeroAddress
			}

			// gasUsed := getGasUsed(traceResult)
			// fmt.Printf("txhash: %v from: %v gasUsed: %v\n", tx.Hash(), from, gasUsed)
			// recordIncrementEOA(from, big.NewInt(int64(gasUsed)))

			recursivelyGetDeployedContracts(traceResult, tx.Hash(), block.Number(), from)
		}
		deploymentScanLog.Printf("Done debug_traceTransaction for block %v\n", blockNum)
	}

}

func processBlockTrace(blockTraces []shared.BlockTrace, callerEOA common.Address, blockNum uint64) ([]contractTableData, []contractSelfdestructData) {
	var contractDatas []contractTableData
	var contractSelfdestructDatas []contractSelfdestructData
	for _, trace := range blockTraces {
		if len(trace.TraceAddress) == 0 { // traceAddress == [] means its the top-level, initial EOA call
			callerEOA = trace.Action.From

			// record eoa total gas used
			// gasUsed := trace.Result.GasUsed

			// fmt.Printf("from: %v txhash: %v gasUsed: %v trace.Action.Gas: %v\n", callerEOA, trace.TransactionHash, gasUsed, trace.Action.Gas)
			// recordIncrementEOA(callerEOA, (*big.Int)(gasUsed))
		}

		// dont proceed with recording contract data if reverts
		if trace.Error != "" {
			continue
		}

		switch trace.Type {
		case "create": // contract created
			contractAddr := trace.Result.Address
			bytecode := trace.Result.Code
			deployerContract := trace.Action.From

			// if int64(psqlWaitGroup.GetCount()) > psqlWaitGroupMaximum {
			// 	deploymentScanLog.Printf("psqlWaitGroup - waiting...")

			// 	for {
			// 		if !(int64(psqlWaitGroup.GetCount()) > psqlWaitGroupMaximum) {
			// 			break
			// 		}
			// 		time.Sleep(1 * time.Second)
			// 		// fmt.Printf("psqlWaitGroup.GetCount(): %v\n", psqlWaitGroup.GetCount())
			// 	}

			// 	deploymentScanLog.Printf("psqlWaitGroup - done waiting")
			// }

			// fmt.Printf("psqlWaitGroup.GetCount(): %v\n", psqlWaitGroup.GetCount())

			// go asyncRecordContractToPSQL(contractAddr, bytecode, deployerContract, callerEOA, big.NewInt(int64(blockNum)))
			contractDatas = append(contractDatas, contractTableData{
				Address:          contractAddr,
				Bytecode:         bytecode,
				DeployerContract: deployerContract,
				DeployerEOA:      callerEOA,
				BlockNum:         big.NewInt(int64(blockNum)),
			})
		case "suicide": // contract self-destruct
			contractAddr := trace.Action.Address
			blockNum := trace.BlockNumber

			contractSelfdestructDatas = append(contractSelfdestructDatas, contractSelfdestructData{
				Address:              contractAddr,
				SelfDestructBlocknum: blockNum,
			})
			// go recordSelfDestructToPSQL(contractAddr, blockNum)
		}
	}

	return contractDatas, contractSelfdestructDatas
}

func getGasUsed(txTraceResult shared.TraceResult) uint64 {
	total := txTraceResult.GasUsed
	for _, tr := range txTraceResult.Calls {
		total += getGasUsed(tr)
	}
	return total
}

// only called with data from debug_traceTransaction
// which is only used as a fallback when Trace_block fails
func recursivelyGetDeployedContracts(txTraceResult shared.TraceResult, txHash common.Hash, blockNum *big.Int, callerEOA common.Address, deployedContracts ...common.Address) []common.Address {
	switch txTraceResult.Type {
	case "CALL", "DELEGATECALL":
		// go through each internal call's trace result (if any) and recurse
		for _, tr := range txTraceResult.Calls {
			deployedContracts = recursivelyGetDeployedContracts(tr, txHash, blockNum, callerEOA, deployedContracts...)
		}
	case "CREATE", "CREATE2":
		deployerContract := txTraceResult.From
		contract := txTraceResult.To
		bytecode := txTraceResult.Output

		recordContractToPSQL(contractTableData{
			Address:          contract,
			Bytecode:         bytecode,
			DeployerContract: deployerContract,
			DeployerEOA:      callerEOA,
			BlockNum:         blockNum,
		})

		deploymentScanLog.Printf("recursive - Recorded contract: %v deployerContract: %v callerEOA: %v\n", contract, deployerContract, callerEOA)
	default:
	}
	return deployedContracts
}

// call this in async
func asyncRecordContractToPSQL(contractAddr common.Address, bytecode []byte, deployerContract common.Address, deployerEOA common.Address, blockNum *big.Int) {
	psqlWaitGroup.Add(1)
	recordContractToPSQL(contractTableData{
		Address:          contractAddr,
		Bytecode:         bytecode,
		DeployerContract: deployerContract,
		DeployerEOA:      deployerEOA,
		BlockNum:         blockNum,
	})
	psqlWaitGroup.Done()
}

func recordContractToPSQL(d contractTableData) {

	bytecodeHash := crypto.Keccak256Hash(d.Bytecode)
	ctx := context.Background()

	// deploymentScanLog.Printf("psql - recording contract addr: %v bytecode hsh: %v\n", contractAddr, bytecodeHash)

	tx, err := dbpool.Begin(ctx)
	if err != nil {
		deploymentScanLog.Fatalf("dbpool.Begin err: %v\n", err)
	}
	defer tx.Rollback(ctx)

	// insert contract -> bytecode hash
	// and blocknum contract was deployed
	if d.DeployerEOA == d.DeployerContract {
		// EOA deployed, only record EOA
		contractQuery := `
		INSERT INTO contracts (address, bytecode_hash, blocknum, deployer_eoa_address)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (address) DO UPDATE
		SET
		blocknum=EXCLUDED.blocknum,
		deployer_eoa_address=EXCLUDED.deployer_eoa_address
		`
		_, err = tx.Exec(ctx, contractQuery, d.Address.Hex(), bytecodeHash.Hex(), d.BlockNum, d.DeployerEOA.Hex())
		if err != nil {
			log.Fatal("Error inserting into contracts table:", err)
		}
	} else {
		// EOA invoked contract which deployed, record both
		contractQuery := `
		INSERT INTO contracts (address, bytecode_hash, blocknum, deployer_eoa_address, deployer_contract_address)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (address) DO UPDATE
		SET
		blocknum=EXCLUDED.blocknum,
		deployer_eoa_address=EXCLUDED.deployer_eoa_address,
		deployer_contract_address=EXCLUDED.deployer_contract_address
		`
		_, err = tx.Exec(ctx, contractQuery, d.Address.Hex(), bytecodeHash.Hex(), d.BlockNum, d.DeployerEOA.Hex(), d.DeployerContract.Hex())
		if err != nil {
			log.Fatal("Error inserting into contracts table:", err)
		}
	}

	// insert bytecode hash -> bytecode
	bytecodeQuery := `
		INSERT INTO bytecodes (bytecode_hash, bytecode)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`
	_, err = tx.Exec(context.Background(), bytecodeQuery, bytecodeHash.Hex(), []byte(d.Bytecode))
	if err != nil {
		log.Fatal("Error inserting into bytecodes table:", err)
	}

	// commit changes
	err = tx.Commit(ctx)
	if err != nil {
		deploymentScanLog.Fatal(err)
	}
}

func batchRecordContractToPSQL(contractDatas []contractTableData) {
	ctx := context.Background()

	for i := 0; i < psqlDeadlockRetryTimes; i++ {

		tx, err := dbpool.Begin(ctx)
		if err != nil {
			deploymentScanLog.Fatalf("dbpool.Begin err: %v\n", err)
		}
		defer tx.Rollback(ctx)

		var success bool
		for _, d := range contractDatas {

			bytecodeHash := crypto.Keccak256Hash(d.Bytecode)

			success = true
			for i := 0; i < psqlDeadlockRetryTimes; i++ {

				// insert contract -> bytecode hash
				// and blocknum contract was deployed
				if d.DeployerEOA == d.DeployerContract {
					// EOA deployed, only record EOA
					contractQuery := `
		INSERT INTO contracts (address, bytecode_hash, blocknum, deployer_eoa_address)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (address) DO UPDATE
		SET
		blocknum=EXCLUDED.blocknum,
		deployer_eoa_address=EXCLUDED.deployer_eoa_address
		`
					_, err = tx.Exec(ctx, contractQuery, d.Address.Hex(), bytecodeHash.Hex(), d.BlockNum, d.DeployerEOA.Hex())
					if err != nil {
						// log.Fatal("Error inserting into contracts table:", err)
						deploymentScanLog.Printf("Error inserting into contracts table: %v", err)
						continue
					}
				} else {
					// EOA invoked contract which deployed, record both
					contractQuery := `
		INSERT INTO contracts (address, bytecode_hash, blocknum, deployer_eoa_address, deployer_contract_address)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (address) DO UPDATE
		SET
		blocknum=EXCLUDED.blocknum,
		deployer_eoa_address=EXCLUDED.deployer_eoa_address,
		deployer_contract_address=EXCLUDED.deployer_contract_address
		`
					_, err = tx.Exec(ctx, contractQuery, d.Address.Hex(), bytecodeHash.Hex(), d.BlockNum, d.DeployerEOA.Hex(), d.DeployerContract.Hex())
					if err != nil {
						// log.Fatal("Error inserting into contracts table:", err)
						deploymentScanLog.Printf("Error inserting into contracts table: %v", err)
						continue
					}
				}

				// insert bytecode hash -> bytecode
				bytecodeQuery := `
		INSERT INTO bytecodes (bytecode_hash, bytecode)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`
				_, err = tx.Exec(context.Background(), bytecodeQuery, bytecodeHash.Hex(), []byte(d.Bytecode))
				if err != nil {
					// log.Fatal("Error inserting into bytecodes table:", err)
					deploymentScanLog.Printf("batchRecordContractToPSQL - error inserting into contracts table: %v i: %v", err, i)
					continue
				}
				success = true
				break
			}
			if !success {
				deploymentScanLog.Printf("(inner) batch record contracts - all retrials exhausted blocknum %v\n", contractDatas[0].BlockNum)
				// deploymentScanLog.Fatalf("(inner) batch record contracts - all retrials exhausted blocknum %v\n", contractDatas[0].BlockNum)
				break
			}
		}

		if !success {
			continue
		}

		// commit changes
		err = tx.Commit(ctx)
		if err != nil {
			// deploymentScanLog.Fatal(err)
			deploymentScanLog.Printf("batch record contract err: %v i: %v blocknum %v\n", err, i, contractDatas[0].BlockNum)
			continue
		}

		deploymentScanLog.Printf("contracts record - finished block %v\n", contractDatas[0].BlockNum)
		return
	}
	deploymentScanLog.Fatalf("batch record contracts - all retrials exhausted blocknum %v\n", contractDatas[0].BlockNum)
}

func recordSelfDestructToPSQL(contractAddr common.Address, blockNum *big.Int) {
	ctx := context.Background()

	deploymentScanLog.Printf("recording selfdestruct addr %v blocknum %v\n", contractAddr, blockNum)

	tx, err := dbpool.Begin(ctx)
	if err != nil {
		deploymentScanLog.Fatalf("dbpool.Begin err: %v\n", err)
	}
	defer tx.Rollback(ctx)

	contractQuery := `
	INSERT INTO contracts (address, self_destruct_blocknum)
	VALUES ($1, $2)
	ON CONFLICT (address) DO UPDATE
	SET
	self_destruct_blocknum=EXCLUDED.self_destruct_blocknum
	`
	_, err = tx.Exec(ctx, contractQuery, contractAddr.Hex(), blockNum)
	if err != nil {
		log.Fatal("recordSelfDestructToPSQL - error inserting into contracts table:", err)
	}

	// commit changes
	tx.Commit(ctx)
}

func batchRecordSelfDestruct(contractSelfdestructDatas []contractSelfdestructData) {
	ctx := context.Background()

	for i := 0; i < psqlDeadlockRetryTimes; i++ {

		tx, err := dbpool.Begin(ctx)
		if err != nil {
			deploymentScanLog.Fatalf("dbpool.Begin err: %v\n", err)
		}
		defer tx.Rollback(ctx)

		deploymentScanLog.Printf("batch record selfdestruct len %v\n", len(contractSelfdestructDatas))

		var success bool

		for _, d := range contractSelfdestructDatas {

			success = false
			for i := 0; i < psqlDeadlockRetryTimes; i++ {
				contractQuery := `
	INSERT INTO contracts (address, self_destruct_blocknum)
	VALUES ($1, $2)
	ON CONFLICT (address) DO UPDATE
	SET
	self_destruct_blocknum=EXCLUDED.self_destruct_blocknum
	`
				_, err = tx.Exec(ctx, contractQuery, d.Address.Hex(), d.SelfDestructBlocknum)
				if err != nil {
					// log.Fatal("recordSelfDestructToPSQL - error inserting into contracts table:", err)
					deploymentScanLog.Printf("recordSelfDestructToPSQL - error inserting into contracts table: %v i: %v", err, i)
					continue
				}
				success = true
				break
			}
			if !success {
				// deploymentScanLog.Fatalf("(inner) batch record selfdestruct - all retrials exhausted SelfDestructBlocknum %v\n", contractSelfdestructDatas[0].SelfDestructBlocknum)
				deploymentScanLog.Printf("(inner) batch record selfdestruct - all retrials exhausted SelfDestructBlocknum %v\n", contractSelfdestructDatas[0].SelfDestructBlocknum)
				break
			}
		}

		if !success {
			continue
		}

		// commit changes
		err = tx.Commit(ctx)
		if err != nil {
			// deploymentScanLog.Fatal(err)
			deploymentScanLog.Printf("batch record selfdestruct err: %v i: %v SelfDestructBlocknum %v\n", err, i, contractSelfdestructDatas[0].SelfDestructBlocknum)
			continue
		}

		deploymentScanLog.Printf("selfdestruct record - finished block %v\n", contractSelfdestructDatas[0].SelfDestructBlocknum)
		return
	}
	deploymentScanLog.Fatalf("batch record selfdestruct - all retrials exhausted SelfDestructBlocknum %v\n", contractSelfdestructDatas[0].SelfDestructBlocknum)
}

func recordBlockTimestamp(blockNum *big.Int, timestamp time.Time) {
	ctx := context.Background()

	tx, err := dbpool.Begin(ctx)
	if err != nil {
		deploymentScanLog.Fatalf("dbpool.Begin err: %v\n", err)
	}
	defer tx.Rollback(ctx)

	// insert block num -> timestamp
	query := `
		INSERT INTO block_timestamps (blocknum, timestamp)
		VALUES ($1, $2)
		ON CONFLICT (blocknum) DO NOTHING
		`
	_, err = tx.Exec(ctx, query, blockNum, timestamp)
	if err != nil {
		log.Fatal("Error inserting into block_timestamps table:", err)
	}

	// commit changes
	err = tx.Commit(ctx)
	if err != nil {
		deploymentScanLog.Fatal(err)
	}
}

func (p action) FinishedDeploymentScan() {
	shared.BlockWaitGroup.Wait()
	deploymentScanLog.Printf("Finished!")
}
