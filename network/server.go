package network

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"github.com/OCAX-labs/rfqrelayer/api"
	"github.com/OCAX-labs/rfqrelayer/common"
	"github.com/OCAX-labs/rfqrelayer/core"
	"github.com/OCAX-labs/rfqrelayer/core/types"
	cryptoocax "github.com/OCAX-labs/rfqrelayer/crypto/ocax"
	"github.com/OCAX-labs/rfqrelayer/db/pebble"
	"github.com/go-kit/log"
)

const (
	devDb    = "./.devdb"
	cache    = 1048
	handles  = 2
	readonly = false
)

var (
	defaultBlockTime = 5 * time.Second

	// Message coloring for Debug
	Reset  = "\033[0m"
	Red    = "\033[31m"
	Green  = "\033[32m"
	Yellow = "\033[33m"
	Blue   = "\033[34m"
	Purple = "\033[35m"
	Cyan   = "\033[36m"
	Gray   = "\033[37m"
	White  = "\033[97m"
)

type ServerOptions struct {
	APIListenAddr string
	SeedNodes     []string
	ListenAddr    string
	TCPTransport  *TCPTransport
	ID            string
	Logger        log.Logger
	RPCDecodeFunc RPCDecodeFunc
	RPCProcessor  RPCProcessor
	BlockTime     time.Duration
	PrivateKey    *cryptoocax.PrivateKey
}

type Server struct {
	TCPTransport *TCPTransport
	peerCh       chan (*TCPPeer)

	mu      sync.RWMutex
	peerMap map[net.Addr]*TCPPeer
	txChan  chan *types.Transaction

	ServerOptions
	memPool     *TxPool
	chain       *core.Blockchain
	isValidator bool
	rpcCh       chan RPC
	quitCh      chan struct{} // options

	ctx        context.Context
	cancelFunc context.CancelFunc
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.BlockTime == time.Duration(0) {
		options.BlockTime = defaultBlockTime
	}
	if options.RPCDecodeFunc == nil {
		options.RPCDecodeFunc = DefaultRPCDecodeFunc
	}
	if options.Logger == nil {
		options.Logger = log.NewLogfmtLogger(os.Stderr)
		options.Logger = log.With(options.Logger, "addr", options.ID)
	}
	log.Logger(options.Logger).Log("msg", "starting node", "id", options.ID)
	// Delete the database directory if it exists
	dbPath := fmt.Sprintf("./.%s.db", options.ID)
	err := deleteDirectoryIfExists(dbPath)
	if err != nil {
		panic(fmt.Sprintf("failed to delete test database directory: %v", err))
	}

	// Create the database
	db, err := pebble.New(dbPath, cache, handles, "rfq", readonly)
	if err != nil {
		return nil, err
	}

	chain, err := core.NewBlockchain(options.Logger, genesisBlock(), db, options.PrivateKey != nil)
	if err != nil {
		return nil, err
	}

	// channel used between json rpc api and the node server
	txChan := make(chan *types.Transaction)
	//
	if len(options.APIListenAddr) > 0 {
		apiServerCfg := api.ServerConfig{
			Logger:     options.Logger,
			ListenAddr: options.APIListenAddr,
		}
		apiServer := api.NewServer(apiServerCfg, chain, txChan)

		go apiServer.Start()

		options.Logger.Log("msg", "JSON API running", "addr", options.APIListenAddr)
	}

	peerCh := make(chan *TCPPeer)
	rpcCh := make(chan RPC, 2048)
	tr := NewTCPTransport(options.ID, options.ListenAddr, peerCh, rpcCh)

	ctx, cancelFunc := context.WithCancel(context.Background())

	s := &Server{
		TCPTransport:  tr,
		peerCh:        peerCh,
		peerMap:       make(map[net.Addr]*TCPPeer),
		ServerOptions: options,
		chain:         chain,
		memPool:       NewTxPool(1000),
		isValidator:   options.PrivateKey != nil,
		rpcCh:         rpcCh,
		quitCh:        make(chan struct{}, 1),
		txChan:        txChan,

		// for broadcasting status messages
		ctx:        ctx,
		cancelFunc: cancelFunc,
	}

	s.TCPTransport.peerCh = peerCh

	if s.RPCProcessor == nil {
		s.RPCProcessor = s
	}
	if s.isValidator {
		go func() {
			s.validatorLoop()
		}()
		go func() {
			time.Sleep(time.Second * 10)
			s.statusLoop()
		}()
	}

	return s, nil
}

func (s *Server) bootstrapNetwork() {
	for _, addr := range s.SeedNodes {

		go func(addr string) {

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				s.Logger.Log("err", err)
				return
			}
			peerCtx, cancel := context.WithCancel(context.Background()) // or pass in the server's context if it exists
			peer := &TCPPeer{
				conn:       conn,
				ctx:        peerCtx,
				cancelFunc: cancel,
			}

			s.peerCh <- peer

		}(addr)

	}
}

var ErrBlockKnown = errors.New("block already known")

func (s *Server) Start() {
	s.TCPTransport.Start()

	time.Sleep(time.Second * 2)

	s.bootstrapNetwork()

	s.Logger.Log("accepting tcp on", s.ListenAddr, "id", s.ID)
	var wg sync.WaitGroup

free:
	for {
		errors := make(chan error)
		select {
		case peer := <-s.peerCh:
			s.peerMap[peer.conn.RemoteAddr()] = peer
			peer.transport = s.TCPTransport

			s.Logger.Log("msg", "new peer added", "outgoing", peer.Outgoing, "addr", peer.conn.RemoteAddr())

			wg.Add(1)
			go func() {
				defer wg.Done()
				peer.readLoop(s.rpcCh, errors)
			}()
			go handleErrors(errors, s.Logger)

			// TODO: Remove this due to new status loop
			// if err := s.sendGetStatusMessage(peer); err != nil {
			// 	fmt.Printf("Error sending get status message: %+v\n", err)
			// 	s.Logger.Log("err", err)
			// 	continue
			// }

		case tx := <-s.txChan:
			if err := s.processTransaction(tx); err != nil {
				s.Logger.Log("TX err", err)
			}

			s.Logger.Log("msg", "new transaction received", "tx", tx)
		case rpc := <-s.rpcCh:
			msg, err := s.RPCDecodeFunc(rpc)
			fmt.Printf(Purple+"XXXX Received msg [%+v]"+Reset+"\n", msg.Data)
			if err != nil {
				s.Logger.Log("err", err)
				continue
			}
			if err := s.RPCProcessor.ProcessMessage(msg); err != nil {
				if err != ErrBlockKnown {
					s.Logger.Log("err", err)
				}
			}
		case <-s.quitCh:
			wg.Wait()
			close(errors)
			break free
		}
	}

	s.Logger.Log("msg", "server stopped")
}

func handleErrors(errors <-chan error, logger log.Logger) {
	for err := range errors {
		logger.Log("An error occurred in readloop", err)
	}
}

func (s *Server) validatorLoop() {
	ticker := time.NewTicker(s.BlockTime)

	s.Logger.Log("msg", "Starting validator loop", "blockTime", s.BlockTime)

	for {
		<-ticker.C
		s.CreateNewBlock()
	}
}

func (s *Server) ProcessMessage(msg *DecodeMessage) error {
	switch t := msg.Data.(type) {
	case *types.Transaction:
		fmt.Println("TX")
		return s.processTransaction(t)
	case *types.Block:
		return s.processBlock(t)
	case *GetStatusMessage:
		return s.processGetStatusMessage(msg.From, t)
	case *StatusMessage:
		return s.processStatusMessage(msg.From, t)
	case *GetBlocksMessage:
		fmt.Printf(Green+"GET BLOCKS MESSAGE - RECEIVED[%+v]: => from %+v t: %+v"+Reset+"\n", s.ID, msg.ID, t)
		return s.processGetBlocksMessage(msg.From, t)
	case *BlocksMessage:
		fmt.Printf(Yellow+"PROCESSBLOCKS MESSAGE - RECEIVED[%+v]: => from %+v t: %+v"+Reset+"\n", s.ID, msg.ID, t)
		return s.processBlocksMessage(msg.From, t)
	default:
		fmt.Printf(Yellow+"UNKNOWN MESSAGE TYPE: %+v"+Reset+"\n", t)

	}

	return nil
}

// GGG
func (s *Server) processGetBlocksMessage(from net.Addr, data *GetBlocksMessage) error {
	// s.Logger.Log("msg", "received GET BLOCKS msg", "from", from, "FromBlock", data.From, "ToBlock", data.To)

	var (
		fullBlocks       = []*FullBlock{}
		ourHeadersLength = uint64(len(s.chain.Headers()))
	)

	// Peovide all blocks up to our current height
	if data.From <= ourHeadersLength && data.To <= ourHeadersLength {
		for i := int(data.To); i <= int(data.From); i++ {
			block, err := s.chain.GetBlock(big.NewInt(int64(i)))
			if err != nil {
				return err
			}
			header := block.Header()
			fullBlocks = append(fullBlocks, &FullBlock{Block: block, Header: header})
		}
	}

	blocksMsg := &BlocksMessage{
		Blocks: fullBlocks,
	}

	buf := new(bytes.Buffer)
	if err := gob.NewEncoder(buf).Encode(blocksMsg); err != nil {
		return err
	}

	msg := NewMessage(MessageTypeBlocks, buf.Bytes(), s.ID)

	// s.mu.RLock()
	// defer s.mu.RUnlock()
	peer, ok := s.peerMap[from]
	if !ok {
		return fmt.Errorf("peer not found")
	}

	if err := peer.Send(msg); err != nil {
		return err
	}
	return nil
}

func (s *Server) sendGetStatusMessage(peer *TCPPeer) error {
	var (
		getStatusMsg = new(GetStatusMessage)
		buf          = new(bytes.Buffer)
	)

	if err := gob.NewEncoder(buf).Encode(getStatusMsg); err != nil {
		return err
	}

	msg := NewMessage(MessageTypeGetStatus, buf.Bytes(), s.ID)
	return peer.Send(msg)
}

func (s *Server) broadcast(payload []byte) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for netAddr, peer := range s.peerMap {
		if err := peer.SendBytesPayload(payload); err != nil {
			fmt.Printf("Error sending to peer: %+v\n", err)
			s.Logger.Log("err", err, "addr", netAddr)
		}

	}
	return nil
}

func (s *Server) processBlocksMessage(from net.Addr, data *BlocksMessage) error {
	fmt.Printf(Cyan+"processing incoming msg: %+v"+Reset+"\n", data)
	for i := 0; i < len(data.Blocks); i++ {
		header := data.Blocks[i].Header
		block := data.Blocks[i].Block
		newBlock := types.NewBlockWithHeader(header).WithBody(block.Transactions(), block.Validator)
		fmt.Printf(Yellow+"newBlock [%d]: %+v"+Reset+"\n", i, newBlock)
		// fmt.Printf(Purple+"block.header [%d]: %+v"+Reset+"\n", i, block.Header())
		if err := s.chain.VerifyBlock(newBlock); err != nil {
			s.Logger.Log("err", err)
			continue
		}

	}

	// for _, block := range data.Blocks {
	// 	block := block.Block.WithHeader(block.Header)
	// 	if err := s.chain.VerifyBlock(block); err != nil {
	// 		fmt.Printf("BLOCK ERROR: %s\n", err)
	// 		continue
	// 		// s.Logger.Log("err", err)
	// 	}
	// }

	return nil
}

func (s *Server) processStatusMessage(from net.Addr, data *StatusMessage) error {
	// If I am not a validator I need block 0
	myHeadersLength := int64(len(s.chain.Headers()))
	if data.CurrentLength < myHeadersLength {
		s.Logger.Log("msg", "No sync: blockheight to low", "our headers len", myHeadersLength, "your headers len", data.CurrentLength, "addr", s.ID)
		return nil
	} // this remote has blocks we can sync}

	if !s.isValidator && myHeadersLength < data.CurrentLength {
		go s.requestBlocksLoop(from, data.CurrentLength)
	}
	return nil
}

func (s *Server) statusLoop() {
	ticker := time.NewTicker(defaultBlockTime)
	lastBroadcastHeight := s.chain.CurrentBlock().Height

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			currentHeight := s.chain.CurrentBlock().Height
			if currentHeight != lastBroadcastHeight {
				buf := new(bytes.Buffer)
				status := s.createStatusMessage(buf) // This should include the current block height
				for _, peer := range s.peerMap {
					fmt.Printf(Cyan+"[%s] - node: sending status message to: [%s] => %+v"+Reset+"\n", s.ID, peer.conn.RemoteAddr(), status)
					_ = peer.Send(status)
				}
				lastBroadcastHeight = currentHeight
			}
		}
	}
}

func (s *Server) processGetStatusMessage(from net.Addr, data *GetStatusMessage) error {
	s.Logger.Log("ID", s.ID, "msg", "received GET STATUS msg from", "addr", from)

	buf := new(bytes.Buffer)
	msg := s.createStatusMessage(buf)
	s.mu.RLock()
	defer s.mu.RUnlock()
	peer, ok := s.peerMap[from]
	if !ok {
		return fmt.Errorf("peer not found")
	}

	return peer.Send(msg)
}

func (s *Server) createStatusMessage(buf *bytes.Buffer) *Message {
	statusMsg := &StatusMessage{
		CurrentLength: int64(len(s.chain.Headers())),
		ID:            s.ID,
	}

	if err := gob.NewEncoder(buf).Encode(statusMsg); err != nil {
		s.Logger.Log("err", err)
		return nil
	}
	msg := NewMessage(MessageTypeStatus, buf.Bytes(), s.ID)

	return msg
}

func (s *Server) processBlock(b *types.Block) error {
	fmt.Printf("processBlock: %+v\n", b)
	if err := s.chain.VerifyBlock(b); err != nil {
		return err
	}

	go s.broadcastBlock(b)

	return nil
}

func (s *Server) processTransaction(tx *types.Transaction) error {
	hash := tx.Hash()

	if s.memPool.Contains(hash) {
		return nil
	}

	// if err := tx.Verify(); err != nil {
	// 	return err
	// }

	// s.Logger.Log(
	// 	"msg", "added new tx to pool",
	// 	"hash", hash,
	// 	"mempool len", s.memPool.PendingCount(),
	// )

	go s.broadcastTx(tx)

	s.memPool.Add(tx)
	return nil
}

// TODO: stop syncing when at highest block
func (s *Server) requestBlocksLoop(peer net.Addr, blocksIndex int64) error {
	ticker := time.NewTicker(6 * time.Second)

	for {
		headersLength := len(s.chain.Headers())
		// blocksIndex := int64(headersLength)
		if headersLength >= int(blocksIndex) {
			s.Logger.Log("msg", "finished syncing", "addr", peer)
			return nil
		}

		s.Logger.Log("msg", "requesting blocks", "requesting headers index", headersLength, "addr", peer)

		getBlocksMsg := &GetBlocksMessage{
			From: uint64(blocksIndex),
			To:   uint64(headersLength),
		}
		buf := new(bytes.Buffer)
		if err := gob.NewEncoder(buf).Encode(getBlocksMsg); err != nil {
			return err
		}

		s.mu.RLock()
		defer s.mu.RUnlock()

		msg := NewMessage(MessageTypeGetBlocks, buf.Bytes(), s.ID)
		peer, ok := s.peerMap[peer]
		if !ok {
			return fmt.Errorf("peer %+s not found", peer.conn.RemoteAddr())
		}

		if err := peer.Send(msg); err != nil {
			s.Logger.Log("error", "failed to send to peer", "err", err, "peer", peer.conn.RemoteAddr())
		}

		<-ticker.C
	}
}

func (s *Server) broadcastBlock(b *types.Block) error {
	buf := &bytes.Buffer{}
	// if err := b.Encode(common.NewGobBlockEncoder(buf)); err != nil {
	// 	return err
	// }
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := NewMessage(MessageTypeBlock, buf.Bytes(), s.ID)

	return s.broadcast(msg.Bytes())
}

func (s *Server) broadcastTx(tx *types.Transaction) error {
	buf := &bytes.Buffer{}
	// if err := tx.Encode(core.NewRLPTxEncoder(buf)); err != nil {
	// 	return err
	// }

	msg := NewMessage(MessageTypeTx, buf.Bytes(), s.ID)

	return s.broadcast(msg.Bytes())
}

func (s *Server) CreateNewBlock() error {
	// 1. get transactions from mempool
	// 2. create a new block
	currentHeader, err := s.chain.GetHeader(s.chain.Height())
	if err != nil {
		return err
	}

	// TODO: change from adding all txs to pool - limit via some function later
	// To match the tx types
	txx := s.memPool.Pending()

	block, err := types.NewBlockFromPrevHeader(currentHeader, txx)
	if err != nil {
		return err
	}

	if err := block.Sign(*s.PrivateKey); err != nil {
		return err
	}

	if err := s.chain.VerifyBlock(block); err != nil {
		return err
	}

	s.memPool.ClearPending()

	go s.broadcastBlock(block)

	return nil
}

func (s *Server) Stop() {
	s.cancelFunc()
}

func genesisBlock() *types.Block {
	header := &types.Header{
		Version:   1,
		TxHash:    common.Hash{},
		Height:    big.NewInt(0),
		Timestamp: uint64(time.Now().UnixNano()),
	}

	privKey := cryptoocax.GeneratePrivateKey()
	pubKey := privKey.PublicKey()
	// hasher := types.NewOcaxHasher()
	txs := []*types.Transaction{}
	b := types.NewBlock(header, txs, pubKey)
	b.Validator = pubKey

	if err := b.Sign(privKey); err != nil {
		panic(err)
	}

	return b
}

func deleteDirectoryIfExists(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(path)
}