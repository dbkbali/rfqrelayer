package core

import (
	"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/OCAX-labs/rfqrelayer/common"
	"github.com/OCAX-labs/rfqrelayer/core/rawdb"
	"github.com/OCAX-labs/rfqrelayer/core/types"
	"github.com/OCAX-labs/rfqrelayer/rfqdb"
	"github.com/OCAX-labs/rfqrelayer/rfqdb/pebble"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/go-kit/log"
)

type ChainInterface interface {
	GetTxByHash(hash common.Hash) (*types.Transaction, error)
	GetBlockByHash(hash common.Hash) (*types.Block, error)
	GetBlock(height *big.Int) (*types.Block, error)
	GetBlockHeader(height *big.Int) (*types.Header, error)
	GetRFQRequests() ([]*types.RFQRequest, error)
	WriteRFQTxs(tx *types.Transaction) error

	// GetLatestBlock() *types.Block
}

type Blockchain struct {
	logger log.Logger
	// blocks are stored in db tables used to provide fast lookup or rfqs
	db      *pebble.Database
	lock    sync.RWMutex
	headers []*types.Header

	// tracks all open RFQS in memory - these are open RFQS that havent yet received quotes
	// as quotes are received the openRFQS are updated by appending to the quotes array
	openRFQS []*types.Transaction
	// tracks all closed RFQS which are not yet matched in memory
	closedRFQS []*types.Transaction
	// tracks all matched RFQS in memory that are pending settlement
	matchedRFQS []*types.Transaction

	// Abstract tables are used to track rfq data and progress
	rfqRequestsTable rfqdb.Database
	openRFQSTable    rfqdb.Database
	closedRFQSTable  rfqdb.Database
	matchedRFQSTable rfqdb.Database
	settledRFQSTable rfqdb.Database
	quotesTable      rfqdb.Database

	// TODO: Remove this
	txStore map[common.Hash]*types.Transaction
	// blockStore map[common.Hash]*Block
	genesisBlock *types.Block

	validator Validator // TODO: convert to interface

	currentBlock atomic.Pointer[types.Header] // Current head of the chain
	bodyCache    *lru.Cache[common.Hash, *types.Body]
	bodyRLPCache *lru.Cache[common.Hash, rlp.RawValue]

	EventChan EventChan
}

type EventChan chan types.TxEvent

func NewBlockchain(l log.Logger, genesis *types.Block, db *pebble.Database, validator bool) (*Blockchain, error) {

	// initialize tables in the kv store for storing the differnt types of Txs
	rfqRequestsTable := rawdb.NewTable(db, "rfqRequests")
	openRFQSTable := rawdb.NewTable(db, "openRFQs")
	closedRFQSTable := rawdb.NewTable(db, "closeRFQs")
	matchedRFQSTable := rawdb.NewTable(db, "matchedRFQs")
	settledRFQSTable := rawdb.NewTable(db, "settledRFQs")
	quotesTable := rawdb.NewTable(db, "quotes")
	bc := &Blockchain{
		headers: []*types.Header{},
		db:      db,
		logger:  l,

		// tracks all open RFQS in memory
		openRFQS: []*types.Transaction{},
		// tracks all closed RFQS which are not yet matched in memory
		closedRFQS: []*types.Transaction{},
		// tracks all matched RFQS in memory that are pending settlement
		matchedRFQS: []*types.Transaction{},

		// Abstract tables are used for storing each type of transaction in the db
		rfqRequestsTable: rfqRequestsTable,
		openRFQSTable:    openRFQSTable,
		closedRFQSTable:  closedRFQSTable,
		matchedRFQSTable: matchedRFQSTable,
		settledRFQSTable: settledRFQSTable,
		quotesTable:      quotesTable,
		// mapping of OpenRfqs to TxHash for quick lookup retrieval from the db
		txStore: make(map[common.Hash]*types.Transaction),
	}
	bc.EventChan = make(EventChan)

	if validator {
		bc.SetValidator(NewBlockValidator(bc))
		err := bc.addBlockWithoutValidation(genesis)
		if err != nil {
			return nil, err
		}
		genesis, err := bc.GetBlock(big.NewInt(0))
		if err != nil {
			return nil, err
		}
		if genesis == nil {
			return nil, fmt.Errorf("failed to load genesis block")
		}
		bc.genesisBlock = genesis
	}

	bc.currentBlock.Store(nil)

	return bc, nil
}

func (bc *Blockchain) SetValidator(v Validator) {
	bc.validator = v
}

func (bc *Blockchain) VerifyBlock(b *types.Block) error {
	if b == nil {
		return fmt.Errorf("malformed block: is nil")
	}
	if b.Header() == nil {
		return fmt.Errorf("malformed block: header is nil")
	}

	if bc.validator != nil {
		if err := bc.validator.ValidateBlock(b); err != nil {
			return err
		}
	}

	// validate transactions
	for _, tx := range b.Transactions() {
		if err := tx.Verify(); err != nil {
			fmt.Printf("Failed to verify transaction: %+v\n", tx)
			return err
		}

		bc.logger.Log("msg", "Parsing Transactions", "len", len(tx.Data()), "hash", tx.Hash())
	}
	bc.logger.Log("msg", "Verifying block for commit to chain ...", "height", b.Height().String(), "hash", b.Hash().String())

	return bc.addBlockWithoutValidation(b)
}

func (bc *Blockchain) GetBlockByHash(hash common.Hash) (*types.Block, error) {
	bc.lock.Lock()
	defer bc.lock.Unlock()

	// block, ok := bc.blockStore[hash]
	// if !ok {
	// 	return nil, fmt.Errorf("block with hash [%x] not found", hash)
	// }
	return &types.Block{}, nil
}

func (bc *Blockchain) GetTxByHash(hash common.Hash) (*types.Transaction, error) {
	bc.lock.Lock()
	defer bc.lock.Unlock()

	tx, ok := bc.txStore[hash]
	if !ok {
		return nil, fmt.Errorf("transaction with hash [%x] not found", hash)
	}
	return tx, nil
}

func (bc *Blockchain) GetBlock(height *big.Int) (*types.Block, error) {
	currHeight := bc.Height()
	reqHeight := height.Int64()
	if height.Cmp(currHeight) == 1 {
		return nil, fmt.Errorf("blockchain height [%s] is less than requested height [%d]", currHeight.String(), reqHeight)
	}
	bc.lock.RLock()
	blockHeader, err := bc.GetBlockHeader(height)
	if err != nil {
		return nil, err
	}
	block := rawdb.ReadBlock(bc.db, blockHeader.Hash(), uint64(reqHeight))
	if block == nil {
		return nil, fmt.Errorf("block with hash [%x] not found", blockHeader.Hash())
	}
	bc.lock.RUnlock()

	return block, nil
}

func (bc *Blockchain) GetBlockHeader(height *big.Int) (*types.Header, error) {
	if height.Cmp(big.NewInt(int64(len(bc.headers)))) == 1 {
		return nil, fmt.Errorf("blockchain height [%d] is less than requested height [%d]", len(bc.headers), height.Int64())
	}
	return bc.headers[height.Int64()], nil
}

// CurrentBlock retrieves the current head block of the canonical chain. The
// block is retrieved from the blockchain's internal cache.
func (bc *Blockchain) CurrentBlock() *types.Header {
	return bc.currentBlock.Load()
}

func (bc *Blockchain) HasBlock(height *big.Int) bool {
	currHeight := bc.Height()
	switch height.Cmp(currHeight) {
	case 1:
		return false
	case 0:
		return true
	case -1:
		return true
	}
	return false
}

// This is an inmemory list of open rfqs
// TODO: add functions for managing this list on rfq expiry
func (bc *Blockchain) AddOpenRFQTx(kvHash common.Hash, openRFQ *types.Transaction) {
	bc.lock.Lock()
	defer bc.lock.Unlock()
	bc.openRFQS = append(bc.openRFQS, openRFQ)
}

func (bc *Blockchain) Height() *big.Int {
	bc.lock.RLock()
	defer bc.lock.RUnlock()

	headLength := len(bc.headers)
	indexHeight := big.NewInt(int64(headLength - 1))

	return indexHeight
}

func (bc *Blockchain) Headers() []*types.Header {
	return bc.headers
}

func (bc *Blockchain) GetOpenRFQs() []*types.Transaction {
	return bc.openRFQS
}

func (bc *Blockchain) WriteRFQTxs(tx *types.Transaction) error {
	bc.lock.Lock()
	defer bc.lock.Unlock()

	var err error
	// For fast access to RFQ data we also write the transaction to "tables" in the kv store so they can be
	// accessed quickly
	// Note that there is a one to one relationship between rfQRequests types due to the transitions that occur
	// in the rfq process which is as follows:
	// 1. RFQRequestTxType - The original request - if the request is verified and validated a new OpenRFQTxType transaction created
	// 2. OpenRFQTxType - is created when RFQ Quotes can be received and on each receipt the record is updated
	// 3. CloseRFQTxType - is created when the RFQ is closed and ready for Matching by MPC nodes
	// 4. MatchedRFQTxType - is created when the RFQ is matched by MPC nodes and settlement is pending
	// 5. SettledRFQTxType - is created when the RFQ is settled and the transaction is complete
	// transactions 2,3,4,5 will have a 1 to 1 relationship with the original RFQRequestTxType - and are stored in the kv store
	// with the same key as the original RFQRequestTxType - but with a different prefix. Also, the original RFQRequestTxType
	// and quotes are signed by the submitting parties whereas the other types are generated by a validator node and signed by
	// the validator node.
	switch tx.Type() {
	case types.RFQRequestTxType:
		// get the raw signature values
		v, r, s := tx.RawSignatureValues()

		rfqRequest := &types.RFQRequest{
			From: *tx.From(),
			Data: tx.RFQData(),
			V:    v,
			R:    r,
			S:    s,
		}

		// encode the RFQ request to RLP
		encRFQ := new(bytes.Buffer)
		if err := rfqRequest.EncodeRLP(encRFQ); err != nil {
			return err
		}

		// for the original RFQ request we use the hash of the transaction as the key
		// as all other transaction types refer to this RFQ they will be saved in their
		// respective tables with the same key
		err = bc.rfqRequestsTable.Put(tx.Hash().Bytes(), encRFQ.Bytes())
	case types.OpenRFQTxType:
		err = bc.openRFQSTable.Put(tx.ReferenceTxHash().Bytes(), tx.Data())
	case types.ClosedRFQTxType:
		err = bc.closedRFQSTable.Put(tx.ReferenceTxHash().Bytes(), tx.Data())
	case types.MatchedRFQTxType:
		err = bc.matchedRFQSTable.Put(tx.ReferenceTxHash().Bytes(), tx.Data())
	case types.SettledRFQTxType:
		err = bc.settledRFQSTable.Put(tx.ReferenceTxHash().Bytes(), tx.Data())
	case types.QuoteTxType:
		err = bc.quotesTable.Put(tx.ReferenceTxHash().Bytes(), tx.Data())
	default:
		return fmt.Errorf("unknown transaction type: %d", tx.Type())
	}

	if err != nil {
		return fmt.Errorf("error writing transaction to kv store tables: %s", err.Error())
	}
	return nil
}

func (bc *Blockchain) GetRFQRequests() ([]*types.RFQRequest, error) {
	var rfqRequests []*types.RFQRequest

	it := bc.rfqRequestsTable.NewIterator(nil, nil)
	defer it.Release()

	for it.Next() {
		// Decode the RLP-encoded transaction data from the iterator
		txData := it.Value()

		var rfqRequest types.RFQRequest
		if err := rlp.DecodeBytes(txData, &rfqRequest); err != nil {
			return nil, fmt.Errorf("error decoding RFQRequest: %w", err)
		}

		rfqRequests = append(rfqRequests, &rfqRequest)
	}

	// Return any potential iteration error
	if err := it.Error(); err != nil {
		return nil, fmt.Errorf("error iterating over transactions: %w", err)
	}

	return rfqRequests, nil
}

func (bc *Blockchain) addBlockWithoutValidation(b *types.Block) error {
	bc.lock.Lock()
	bc.headers = append(bc.headers, b.Header())
	// write the block which includes all transactions to the kv store
	rawdb.WriteBlock(bc.db, b)

	bc.lock.Unlock()
	bc.logger.Log("msg", "Block saved to the kv store", "hash", b.Hash(), "height", b.Height().String(), "txs", len(b.Transactions()))

	bc.currentBlock.Store(b.Header())

	if len(bc.headers) == 1 {
		bc.logger.Log("msg", "Genesis block added to the chain", "hash", b.Hash(), "height", b.Height().String())
		bc.genesisBlock = b
		return nil
	}

	bc.logger.Log(
		"msg", "BlockAdd",
		"blockhash", b.Hash(),
		"parent", b.ParentHash(),
		"headerHash", b.Header().Hash(),
		"height", b.Height().String(),
		"txs", len(b.Transactions()),
	)
	return nil
}
