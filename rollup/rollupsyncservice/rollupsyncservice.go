package rollupsyncservice

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/scroll-tech/go-ethereum/accounts/abi"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/core"
	"github.com/scroll-tech/go-ethereum/core/rawdb"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/ethdb"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/node"
	"github.com/scroll-tech/go-ethereum/params"

	"github.com/scroll-tech/go-ethereum/rollup/rcfg"
	"github.com/scroll-tech/go-ethereum/rollup/sync_service"
	"github.com/scroll-tech/go-ethereum/rollup/withdrawtrie"
)

const (
	// defaultFetchBlockRange is the number of blocks that we collect in a single eth_getLogs query.
	defaultFetchBlockRange = uint64(100)

	// defaultPollInterval is the frequency at which we query for new rollup event.
	defaultPollInterval = time.Second * 60
)

// RollupSyncService collects ScrollChain batch commit/revert/finalize events and stores metadata into db.
type RollupSyncService struct {
	ctx                           context.Context
	cancel                        context.CancelFunc
	client                        *L1Client
	db                            ethdb.Database
	latestProcessedBlock          uint64
	scrollChainABI                *abi.ABI
	l1CommitBatchEventSignature   common.Hash
	l1RevertBatchEventSignature   common.Hash
	l1FinalizeBatchEventSignature common.Hash
	bc                            *core.BlockChain
}

func NewRollupSyncService(ctx context.Context, genesisConfig *params.ChainConfig, nodeConfig *node.Config, db ethdb.Database, l1Client sync_service.EthClient, bc *core.BlockChain) (*RollupSyncService, error) {
	// terminate if the caller does not provide an L1 client (e.g. in tests)
	if l1Client == nil || (reflect.ValueOf(l1Client).Kind() == reflect.Ptr && reflect.ValueOf(l1Client).IsNil()) {
		log.Warn("No L1 client provided, L1 rollup sync service will not run")
		return nil, nil
	}

	if genesisConfig.Scroll.L1Config == nil {
		return nil, fmt.Errorf("missing L1 config in genesis")
	}

	scrollChainABI, err := scrollChainMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("failed to get scroll chain abi: %w", err)
	}

	client, err := newL1Client(ctx, l1Client, genesisConfig.Scroll.L1Config.L1ChainId, genesisConfig.Scroll.L1Config.ScrollChainAddress, scrollChainABI)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize l1 client: %w", err)
	}

	// Initialize the latestProcessedBlock with the block just before the L1 deployment block.
	// This serves as a default value when there's no L1 rollup events synced in the database.
	latestProcessedBlock := nodeConfig.L1DeploymentBlock - 1

	block := rawdb.ReadRollupEventSyncedL1BlockNumber(db)
	if block != nil {
		// restart from latest synced block number
		latestProcessedBlock = *block
	}

	ctx, cancel := context.WithCancel(ctx)

	service := RollupSyncService{
		ctx:                           ctx,
		cancel:                        cancel,
		client:                        client,
		db:                            db,
		latestProcessedBlock:          latestProcessedBlock,
		scrollChainABI:                scrollChainABI,
		l1CommitBatchEventSignature:   scrollChainABI.Events["CommitBatch"].ID,
		l1RevertBatchEventSignature:   scrollChainABI.Events["RevertBatch"].ID,
		l1FinalizeBatchEventSignature: scrollChainABI.Events["FinalizeBatch"].ID,
		bc:                            bc,
	}

	return &service, nil
}

func (s *RollupSyncService) Start() {
	if s == nil {
		return
	}

	log.Info("Starting rollup event sync background service", "latest processed block", s.latestProcessedBlock)

	go func() {
		t := time.NewTicker(defaultPollInterval)
		defer t.Stop()

		for {
			s.fetchRollupEvents()

			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				continue
			}
		}
	}()
}

func (s *RollupSyncService) Stop() {
	if s == nil {
		return
	}

	log.Info("Stopping rollup event sync background service")

	if s.cancel != nil {
		s.cancel()
	}
}

func (s *RollupSyncService) fetchRollupEvents() {
	latestConfirmed, err := s.client.getLatestFinalizedBlockNumber(s.ctx)
	if err != nil {
		log.Warn("failed to get latest confirmed block number", "err", err)
		return
	}

	log.Trace("Sync service fetchRollupEvents", "latestProcessedBlock", s.latestProcessedBlock, "latestConfirmed", latestConfirmed)

	// query in batches
	for from := s.latestProcessedBlock + 1; from <= latestConfirmed; from += defaultFetchBlockRange {
		if s.ctx.Err() != nil {
			return
		}

		to := from + defaultFetchBlockRange - 1
		if to > latestConfirmed {
			to = latestConfirmed
		}

		logs, err := s.client.fetchRollupEventsInRange(s.ctx, from, to)
		if err != nil {
			// return and retry in next loop
			log.Warn("failed to fetch rollup events in range", "fromBlock", from, "toBlock", to, "err", err)
			return
		}

		if err := s.parseAndUpdateRollupEventLogs(logs, to); err != nil {
			log.Error("failed to parse and update rollup event logs", "err", err)
		}
	}
}

func (s *RollupSyncService) parseAndUpdateRollupEventLogs(logs []types.Log, lastBlock uint64) error {
	for _, vLog := range logs {
		switch vLog.Topics[0] {
		case s.l1CommitBatchEventSignature:
			event := L1CommitBatchEvent{}
			if err := UnpackLog(s.scrollChainABI, event, "CommitBatch", vLog); err != nil {
				return fmt.Errorf("failed to unpack commit rollup event log, err: %w", err)
			}
			batchIndex := vLog.Topics[1].Big().Uint64()
			chunkRanges, err := s.getChunkRanges(batchIndex, &vLog)
			if err != nil {
				return fmt.Errorf("failed to get chunk ranges, err: %w", err)
			}
			rawdb.WriteBatchChunkRanges(s.db, batchIndex, chunkRanges)

		case s.l1RevertBatchEventSignature:
			event := L1RevertBatchEvent{}
			if err := UnpackLog(s.scrollChainABI, event, "RevertBatch", vLog); err != nil {
				return fmt.Errorf("failed to unpack revert rollup event log, err: %w", err)
			}
			batchIndex := vLog.Topics[1].Big().Uint64()
			rawdb.DeleteBatchChunkRanges(s.db, batchIndex)

		case s.l1FinalizeBatchEventSignature:
			event := L1FinalizeBatchEvent{}
			if err := UnpackLog(s.scrollChainABI, event, "FinalizeBatch", vLog); err != nil {
				return fmt.Errorf("failed to unpack finalized rollup event log, err: %w", err)
			}
			batchIndex := event.BatchIndex.Uint64()
			batchHash := event.BatchHash
			stateRoot := event.StateRoot
			withdrawRoot := event.WithdrawRoot

			parentBatchMeta, chunks, err := s.getLocalInfo(batchIndex)
			if err != nil {
				return fmt.Errorf("failed to get local node info, batch index: %v, err: %w", batchIndex, err)
			}

			if err := validateBatch(batchIndex, batchHash, stateRoot, withdrawRoot, parentBatchMeta, chunks); err != nil {
				return fmt.Errorf("fatal: validateBatch failed: batch index: %v, err: %w", batchIndex, err)
			}
			lastChunk := chunks[len(chunks)-1]
			lastBlock := lastChunk.Blocks[len(lastChunk.Blocks)-1]
			rawdb.WriteFinalizedL2BlockNumber(s.db, lastBlock.Header.Number.Uint64())
			rawdb.WriteFinalizedBatchMeta(s.db, batchIndex, s.getFinalizedBatchMeta(batchHash, parentBatchMeta, chunks))

		default:
			return fmt.Errorf("unknown event, topic: %v, tx hash: %v", vLog.Topics[0].Hex(), vLog.TxHash.Hex())
		}
	}

	// note: the batch updates above are idempotent, if we crash
	// before this line and reexecute the previous steps, we will
	// get the same result.
	rawdb.WriteRollupEventSyncedL1BlockNumber(s.db, lastBlock)
	s.latestProcessedBlock = lastBlock

	return nil
}

func (s *RollupSyncService) getFinalizedBatchMeta(batchHash common.Hash, parentBatchMeta *rawdb.FinalizedBatchMeta, chunks []*Chunk) rawdb.FinalizedBatchMeta {
	totalL1MessagePopped := parentBatchMeta.TotalL1MessagePopped
	for _, chunk := range chunks {
		totalL1MessagePopped += chunk.NumL1Messages(totalL1MessagePopped)
	}
	return rawdb.FinalizedBatchMeta{
		BatchHash:            batchHash,
		TotalL1MessagePopped: totalL1MessagePopped,
	}
}

func (s *RollupSyncService) getLocalInfo(batchIndex uint64) (*rawdb.FinalizedBatchMeta, []*Chunk, error) {
	chunkRanges := rawdb.ReadBatchChunkRanges(s.db, batchIndex)
	blocks, err := s.getBlocksInRange(chunkRanges)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get blocks in range, err: %w", err)
	}

	// default to genesis batch meta.
	parentBatchMeta := &rawdb.FinalizedBatchMeta{}
	if batchIndex > 0 {
		parentBatchMeta = rawdb.ReadFinalizedBatchMeta(s.db, batchIndex-1)
	}

	chunks, err := s.convertBlocksToChunks(blocks, chunkRanges)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert blocks to chunks, batch index: %v, chunk ranges: %v, err: %w", batchIndex, chunkRanges, err)
	}
	return parentBatchMeta, chunks, nil
}

func (s *RollupSyncService) getChunkRanges(batchIndex uint64, vLog *types.Log) ([]*rawdb.ChunkBlockRange, error) {
	if batchIndex == 0 {
		return []*rawdb.ChunkBlockRange{{StartBlockNumber: 0, EndBlockNumber: 0}}, nil
	}

	tx, _, err := s.client.client.TransactionByHash(context.Background(), vLog.TxHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction, err: %w", err)
	}

	return s.decodeChunkRanges(tx.Data())
}

// decodeChunkRanges decodes chunks in a batch based on the commit batch transaction's calldata.
// Note: We assume that `commitBatch` is always the outermost call, never an internal transaction.
func (s *RollupSyncService) decodeChunkRanges(txData []byte) ([]*rawdb.ChunkBlockRange, error) {
	decoded, err := s.scrollChainABI.Unpack("commitBatch", txData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode transaction data, err: %w", err)
	}

	if len(decoded) != 4 {
		return nil, fmt.Errorf("invalid decoded length, expected: 4, got: %v,", len(decoded))
	}

	chunks := decoded[2].([]string)
	var chunkRanges []*rawdb.ChunkBlockRange
	startBlockNumber, err := strconv.ParseUint(chunks[0][4:20], 16, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse blockNumber, err: %w", err)
	}

	for _, chunk := range chunks {
		numBlocks, err := strconv.ParseUint(chunk[0:4], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("failed to parse numBlocks, err: %w", err)
		}
		chunkRanges = append(chunkRanges, &rawdb.ChunkBlockRange{
			StartBlockNumber: startBlockNumber,
			EndBlockNumber:   startBlockNumber + numBlocks - 1,
		})
		startBlockNumber += numBlocks
	}

	return chunkRanges, nil
}

// getBlocksInRange retrieves blocks from the blockchain within specified chunk ranges.
func (s *RollupSyncService) getBlocksInRange(chunkRanges []*rawdb.ChunkBlockRange) ([]*types.Block, error) {
	var blocks []*types.Block

	for _, chunkRange := range chunkRanges {
		for i := chunkRange.StartBlockNumber; i <= chunkRange.EndBlockNumber; i++ {
			block := s.bc.GetBlockByNumber(i)
			if block == nil {
				return nil, fmt.Errorf("failed to get block by number: %v", i)
			}
			blocks = append(blocks, block)
		}
	}

	return blocks, nil
}

// convertBlocksToChunks processes and groups blocks into chunks based on the provided chunk ranges.
func (s *RollupSyncService) convertBlocksToChunks(blocks []*types.Block, chunkRanges []*rawdb.ChunkBlockRange) ([]*Chunk, error) {
	if len(blocks) == 0 {
		return nil, fmt.Errorf("invalid arg: empty blocks")
	}

	wrappedBlocks := make([]*WrappedBlock, len(blocks))
	for i, block := range blocks {
		txData := txsToTxsData(block.Transactions())
		state, err := s.bc.StateAt(block.Hash())
		if err != nil {
			return nil, fmt.Errorf("failed to get block state, block: %v, err: %w", block.Hash().Hex(), err)
		}
		withdrawRoot := withdrawtrie.ReadWTRSlot(rcfg.L2MessageQueueAddress, state)
		wrappedBlocks[i] = &WrappedBlock{
			Header:       block.Header(),
			Transactions: txData,
			WithdrawRoot: withdrawRoot,
		}
	}

	minBlockNumber := blocks[0].Header().Number.Uint64()
	var chunks []*Chunk
	for _, cr := range chunkRanges {
		start, end := cr.StartBlockNumber-minBlockNumber, cr.EndBlockNumber-minBlockNumber
		// ensure start and end are within valid range.
		if start < 0 || end >= uint64(len(wrappedBlocks)) || start > end {
			return nil, fmt.Errorf("invalid chunk range, start: %v, end: %v, block len: %v", start, end, len(wrappedBlocks))
		}
		chunk := &Chunk{
			Blocks: wrappedBlocks[start : end+1],
		}
		chunks = append(chunks, chunk)
	}

	return chunks, nil
}

// validateBatch verifies the consistency between l1 contract and l2 node data.
// crash once any consistency check fails.
func validateBatch(batchIndex uint64, batchHash common.Hash, stateRoot common.Hash, withdrawRoot common.Hash, parentBatchMeta *rawdb.FinalizedBatchMeta, chunks []*Chunk) error {
	if len(chunks) == 0 {
		return fmt.Errorf("invalid arg: length of chunks is 0")
	}
	lastChunk := chunks[len(chunks)-1]
	if len(lastChunk.Blocks) == 0 {
		return fmt.Errorf("invalid arg: block number of last chunk is 0")
	}
	lastBlock := lastChunk.Blocks[len(lastChunk.Blocks)-1]
	localWithdrawRoot := lastBlock.WithdrawRoot
	if localWithdrawRoot != withdrawRoot {
		log.Crit("Withdraw root mismatch", "l1 withdraw root", withdrawRoot.Hex(), "l2 withdraw root", localWithdrawRoot.Hex())
	}

	localStateRoot := lastBlock.Header.Root
	if localStateRoot != stateRoot {
		log.Crit("State root mismatch", "l1 state root", stateRoot.Hex(), "l2 state root", localStateRoot.Hex())
	}

	batchHeader, err := NewBatchHeader(batchHeaderVersion, batchIndex, parentBatchMeta.TotalL1MessagePopped, parentBatchMeta.BatchHash, chunks)
	if err != nil {
		return fmt.Errorf("failed to construct batch header, err: %w", err)
	}

	localBatchHash := batchHeader.Hash()
	if localBatchHash != batchHash {
		log.Crit("Batch hash mismatch", "l1 batch hash", batchHash.Hex(), "l2 batch hash", localBatchHash.Hex())
	}

	return nil
}