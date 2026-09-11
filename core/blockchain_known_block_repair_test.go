package core

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/XDCx/tradingstate"
	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus/XDPoS"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/state"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/core/vm"
	"github.com/XinFinOrg/XDPoSChain/crypto"
	"github.com/XinFinOrg/XDPoSChain/params"
)

// newGapChainFixture builds the smallest chain the gap-block tests need: a
// genesis on top of a memory database plus an XDPoS faker engine. The blocks the
// tests want to look like the leftovers of an interrupted insertion are written
// straight into the database, which is the shape writeBlockWithState leaves
// behind when it aborts after persisting the block but before it finishes
// reorganising.
//
// alloc is the genesis allocation: a non-empty one makes the genesis state root
// a real trie node in the database, so blocks executed on top of it have
// readable state, while an empty one is enough for tests that only need the
// genesis root to exist.
func newGapChainFixture(t *testing.T, alloc GenesisAlloc) *BlockChain {
	t.Helper()
	return newGapChainFixtureWithConfig(t, params.TestXDPoSMockChainConfig, alloc)
}

// newGapChainFixtureWithConfig is newGapChainFixture for tests that need their
// own gap schedule.
func newGapChainFixtureWithConfig(t *testing.T, config *params.ChainConfig, alloc GenesisAlloc) *BlockChain {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	genesis := &Genesis{
		Config:    config,
		GasLimit:  10_000_000,
		Alloc:     alloc,
		ExtraData: append(make([]byte, 32), make([]byte, crypto.SignatureLength)...),
	}
	genesis.MustCommit(db)

	engine := XDPoS.NewFaker(db, config)
	if engine == nil {
		t.Fatal("failed to create the XDPoS faker engine")
	}
	t.Cleanup(func() { engine.Stop() })

	// Archive-style cache config so Stop does not try to persist the recent
	// states of the synthetic chain.
	chain, err := NewBlockChain(db, &CacheConfig{TrieDirtyDisabled: true}, genesis, engine, vm.Config{})
	if err != nil {
		t.Fatalf("failed to create the blockchain: %v", err)
	}
	t.Cleanup(func() { chain.Stop() })
	return chain
}

// newKnownBlockFixture is newGapChainFixture with the non-empty allocation the
// tests that execute blocks on top of the genesis need.
func newKnownBlockFixture(t *testing.T) *BlockChain {
	t.Helper()
	return newGapChainFixture(t, GenesisAlloc{common.Address{0x01}: {Balance: big.NewInt(1)}})
}

// syntheticBlock builds an empty block with the given state root. The header is
// never verified here: the tests store these blocks directly, the way a block
// persisted by an aborted insertion already is.
func syntheticBlock(number uint64, parentHash, root common.Hash) *types.Block {
	return types.NewBlockWithHeader(&types.Header{
		Number:      new(big.Int).SetUint64(number),
		ParentHash:  parentHash,
		Root:        root,
		Difficulty:  big.NewInt(1),
		GasLimit:    10_000_000,
		UncleHash:   types.EmptyUncleHash,
		TxHash:      types.EmptyRootHash,
		ReceiptHash: types.EmptyRootHash,
	})
}

// executableBlock returns an empty block on top of parent whose state root is
// the one produced by really executing it, so the block passes the state
// validation of the insertion path. Executing an empty block does change the
// state (the XDPoS processor derives block rewards), so the root can neither be
// copied from the parent nor guessed. The execution result is committed into the
// trie database, which is what makes HasBlockAndFullState accept the block
// afterwards, just as a completed writeBlockWithState leaves it behind.
func executableBlock(t *testing.T, chain *BlockChain, parent *types.Block, number uint64) *types.Block {
	t.Helper()
	statedb, err := chain.StateAt(parent.Root())
	if err != nil {
		t.Fatalf("test setup cannot open the parent state: %v", err)
	}
	block := syntheticBlock(number, parent.Hash(), parent.Root())
	calculated := &CalculatedBlock{block: block}
	if _, _, _, err := chain.processor.ProcessBlockNoValidator(calculated, statedb, nil, chain.vmConfig, nil); err != nil {
		t.Fatalf("test setup cannot execute the synthetic block %d: %v", number, err)
	}
	root, err := statedb.Commit(number, chain.chainConfig.IsEIP158(block.Number()))
	if err != nil {
		t.Fatalf("test setup cannot commit the state of the synthetic block %d: %v", number, err)
	}
	chain.triedb.Reference(root, common.Hash{})
	header := block.Header()
	header.Root = root
	return types.NewBlockWithHeader(header)
}

// publish stores what writeBlockWithState writes before it starts reorganising:
// the block itself and its total difficulty.
func publish(t *testing.T, chain *BlockChain, block *types.Block, td *big.Int) {
	t.Helper()
	rawdb.WriteTd(chain.ChainDb(), block.Hash(), block.NumberU64(), td)
	rawdb.WriteBlock(chain.ChainDb(), block)
}

// TestInsertKnownBlockAheadOfHeadAdvancesHead pins the recovery half of the
// known-block handling: a block already stored together with its full state but
// still above the head must be driven through the insertion path again instead
// of being answered with a silent nil. writeBlockWithState persists the block,
// its receipts and its state before it reorganises, so an aborted reorg leaves
// exactly this shape behind and treating it as done would strand the node on
// the old branch forever.
func TestInsertKnownBlockAheadOfHeadAdvancesHead(t *testing.T) {
	chain := newKnownBlockFixture(t)
	config := params.TestXDPoSMockChainConfig
	genesis := chain.Genesis()
	gapBlockNum := config.XDPoS.Epoch - config.XDPoS.Gap

	// The head is the gap block of the first epoch. Its own state root does not
	// matter: it is only the state the extension below executes against.
	head := syntheticBlock(gapBlockNum, genesis.Hash(), genesis.Root())
	publish(t, chain, head, big.NewInt(1))
	chain.writeHeadBlock(head, true)

	// The known block extends the head and is not a gap block itself, so
	// re-processing it needs no masternode derivation.
	known := executableBlock(t, chain, head, gapBlockNum+1)
	publish(t, chain, known, big.NewInt(2))

	if !chain.HasBlockAndFullState(known.Hash(), known.NumberU64()) {
		t.Fatal("test setup expects the block to be stored with its full state")
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("test setup expects the head to stay at %s, got %s", head.Hash().Hex(), got.Hex())
	}

	headEvents := make(chan ChainHeadEvent, 1)
	sub := chain.SubscribeChainHeadEvent(headEvents)
	defer sub.Unsubscribe()

	if err := chain.InsertBlock(known); err != nil {
		t.Fatalf("re-inserting the known block failed: %v", err)
	}
	if got := chain.CurrentBlock().Hash(); got != known.Hash() {
		t.Fatalf("the known block was swallowed: head is %s, want %s", got.Hex(), known.Hash().Hex())
	}
	select {
	case ev := <-headEvents:
		if ev.Block.Hash() != known.Hash() {
			t.Fatalf("unexpected chain head event for %s", ev.Block.Hash().Hex())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no chain head event was emitted for the recovered block")
	}
}

// TestInsertKnownBlockAheadOfHeadReturnsReorgFailure pins the other half: when
// re-driving the known block reaches the same unusable gap-block state that made
// the original insertion abort, the failure must be reported instead of being
// answered with a silent nil. The head has to stay where it was, because the
// refresh runs before any side effect of the reorg.
func TestInsertKnownBlockAheadOfHeadReturnsReorgFailure(t *testing.T) {
	chain := newKnownBlockFixture(t)
	config := params.TestXDPoSMockChainConfig
	genesis := chain.Genesis()
	gapBlockNum := config.XDPoS.Epoch - config.XDPoS.Gap

	// The branch the node is stuck on.
	head := syntheticBlock(gapBlockNum-1, genesis.Hash(), genesis.Root())
	publish(t, chain, head, big.NewInt(1))
	chain.writeHeadBlock(head, true)

	// The competing branch it should have switched to: head -> gap(450, state
	// gone) -> 451 -> 452, where 452 is the block the aborted insertion had
	// already persisted.
	unreadableRoot := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000dead")
	gap := syntheticBlock(gapBlockNum, head.Hash(), unreadableRoot)
	publish(t, chain, gap, big.NewInt(2))
	mid := syntheticBlock(gapBlockNum+1, gap.Hash(), genesis.Root())
	publish(t, chain, mid, big.NewInt(3))
	known := executableBlock(t, chain, mid, gapBlockNum+2)
	publish(t, chain, known, big.NewInt(4))

	if chain.HasState(unreadableRoot) {
		t.Fatal("test setup expects the gap block state to be absent from the database")
	}
	if !chain.HasBlockAndFullState(known.Hash(), known.NumberU64()) {
		t.Fatal("test setup expects the block to be stored with its full state")
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("test setup expects the head to stay at %s, got %s", head.Hash().Hex(), got.Hex())
	}

	err := chain.InsertBlock(known)
	if err == nil {
		t.Fatalf("expected the known block insertion to fail, head is %s", chain.CurrentBlock().Hash().Hex())
	}
	if !strings.Contains(err.Error(), "failed to update masternodes during reorg") {
		t.Fatalf("the known block insertion failed for the wrong reason: %v", err)
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("a failed reorg moved the head: got %s, want %s", got.Hex(), head.Hash().Hex())
	}
}

// TestInsertKnownBlockAheadOfHeadKeepsFollowingBlock pins the batch half of the
// known-block handling, which the two tests above do not reach: they both go
// through InsertBlock, while this one goes through InsertChain, so the block
// after a skipped known block is produced by the iterator of insertChain.
//
// Skipping a known block that owes no reorganisation must not consume the
// following block with it: the batch is contiguous, so the block after the
// skipped one is still owed to the chain. Answering such a redelivery by
// reporting the whole batch as consumed would strand the chain on the branch
// the head is stuck on.
func TestInsertKnownBlockAheadOfHeadKeepsFollowingBlock(t *testing.T) {
	chain := newKnownBlockFixture(t)
	config := params.TestXDPoSMockChainConfig
	genesis := chain.Genesis()
	gapBlockNum := config.XDPoS.Epoch - config.XDPoS.Gap

	// The branch the head stays on. Its total difficulty is far above the
	// competing branch below, so no reorganisation of that branch is owed.
	head := syntheticBlock(gapBlockNum-1, genesis.Hash(), genesis.Root())
	publish(t, chain, head, big.NewInt(100))
	chain.writeHeadBlock(head, true)

	// A branch the head will never switch to: head -> gap(450) -> 451 -> 452,
	// where 452 is already stored with its full state and 453 is the block a
	// redelivered batch hands over right after it.
	gap := syntheticBlock(gapBlockNum, head.Hash(), genesis.Root())
	publish(t, chain, gap, big.NewInt(20))
	mid := syntheticBlock(gapBlockNum+1, gap.Hash(), genesis.Root())
	publish(t, chain, mid, big.NewInt(50))
	known := executableBlock(t, chain, mid, gapBlockNum+2)
	publish(t, chain, known, big.NewInt(60))
	following := executableBlock(t, chain, known, gapBlockNum+3)

	if chain.knownBlockAwaitsReorg(known) {
		t.Fatal("test setup expects the known block to owe no reorganisation")
	}
	if !chain.HasBlockAndFullState(known.Hash(), known.NumberU64()) {
		t.Fatal("test setup expects the known block to be stored with its full state")
	}
	if chain.HasBlock(following.Hash(), following.NumberU64()) {
		t.Fatal("test setup expects the following block to be unknown")
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("test setup expects the head to stay at %s, got %s", head.Hash().Hex(), got.Hex())
	}

	n, err := chain.InsertChain(types.Blocks{known, following})
	if err != nil {
		t.Fatalf("inserting the batch failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected both blocks of the batch to be accounted for, got %d", n)
	}
	if !chain.HasBlock(following.Hash(), following.NumberU64()) {
		t.Fatalf("the block following the skipped known block was swallowed: %d is not stored", following.NumberU64())
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("a lighter branch moved the head: got %s, want %s", got.Hex(), head.Hash().Hex())
	}
}

// countingProcessor counts how often a block is executed, so a test can pin that
// a redelivery of a known block does not re-execute it.
type countingProcessor struct {
	Processor
	processCalls int
}

func (p *countingProcessor) ProcessBlockNoValidator(block *CalculatedBlock, statedb *state.StateDB, tradingState *tradingstate.TradingStateDB, cfg vm.Config, balanceFee map[common.Address]*big.Int) (types.Receipts, []*types.Log, uint64, error) {
	p.processCalls++
	return p.Processor.ProcessBlockNoValidator(block, statedb, tradingState, cfg, balanceFee)
}

// newRefreshFailingGapBlock builds a chain whose head is the parent of a gap
// block plus a gap block that is stored with its full state but whose
// next-epoch set cannot be derived: the fixture genesis carries no voting
// contract, so DeriveNextEpochMasternodes reports ErrNoCandidates while the
// block itself executes and validates fine. Inserting it therefore reaches the
// extension path's refresh and fails there.
func newRefreshFailingGapBlock(t *testing.T) (*BlockChain, *types.Block, *types.Block) {
	t.Helper()
	chain := newKnownBlockFixture(t)
	config := params.TestXDPoSMockChainConfig
	genesis := chain.Genesis()
	gapBlockNum := config.XDPoS.Epoch - config.XDPoS.Gap

	// The head is the parent of the gap block, so the gap block extends it and
	// the refresh under test is the extension path of writeBlockWithState.
	head := syntheticBlock(gapBlockNum-1, genesis.Hash(), genesis.Root())
	publish(t, chain, head, big.NewInt(1))
	chain.writeHeadBlock(head, true)

	gap := executableBlock(t, chain, head, gapBlockNum)
	publish(t, chain, gap, big.NewInt(2))

	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("test setup expects the head to stay at %s, got %s", head.Hash().Hex(), got.Hex())
	}
	if !chain.HasBlockAndFullState(gap.Hash(), gap.NumberU64()) {
		t.Fatal("test setup expects the gap block to be stored with its full state")
	}
	return chain, head, gap
}

// TestInsertGapBlockRefreshFailureReturnsError pins B1: when the extension path
// cannot derive the next-epoch set of a gap block it must report the failure
// instead of terminating the process through log.Crit, and the head must stay on
// the parent. Before the fix this test would have called os.Exit from log.Crit.
func TestInsertGapBlockRefreshFailureReturnsError(t *testing.T) {
	chain, head, gap := newRefreshFailingGapBlock(t)

	err := chain.InsertBlock(gap)
	if err == nil {
		t.Fatal("expected inserting a gap block whose next-epoch set cannot be derived to fail")
	}
	if !strings.Contains(err.Error(), "failed to update masternodes during writeBlockWithState") {
		t.Fatalf("the insertion failed for the wrong reason: %v", err)
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("a failed refresh moved the head: got %s, want %s", got.Hex(), head.Hash().Hex())
	}
}

// TestInsertGapBlockRefreshFailureDoesNotReExecute pins C2: a redelivery of a
// known block whose re-drive already failed against the current head must be
// answered without executing the block again. The outcome cannot change while
// the head is unchanged, so re-running it on every redelivery would burn CPU and
// I/O with no chance of progress.
func TestInsertGapBlockRefreshFailureDoesNotReExecute(t *testing.T) {
	chain, head, gap := newRefreshFailingGapBlock(t)

	counter := &countingProcessor{Processor: chain.processor}
	chain.processor = counter

	firstErr := chain.InsertBlock(gap)
	if firstErr == nil {
		t.Fatal("expected the first re-drive of the gap block to fail")
	}
	if !strings.Contains(firstErr.Error(), "failed to update masternodes during writeBlockWithState") {
		t.Fatalf("the first re-drive failed for the wrong reason: %v", firstErr)
	}
	executions := counter.processCalls
	if executions == 0 {
		t.Fatal("test setup expects the first re-drive to execute the block")
	}
	if !chain.redriveBlocked(gap) {
		t.Fatal("the failed re-drive must be remembered while the head is unchanged")
	}

	if err := chain.InsertBlock(gap); err != nil {
		t.Fatalf("a redelivery of a known block whose re-drive already failed must be ignored, got %v", err)
	}
	if counter.processCalls != executions {
		t.Fatalf("the block was executed again on the redelivery: %d -> %d calls", executions, counter.processCalls)
	}
	if got := chain.CurrentBlock().Hash(); got != head.Hash() {
		t.Fatalf("the head moved on a redelivery: got %s, want %s", got.Hex(), head.Hash().Hex())
	}
}

// TestExternTdBeatsHead pins the shared total-difficulty rule both the write path
// and the known-block check consult: a strictly higher total difficulty wins, an
// equal one is split by block number, a lower one never does.
func TestExternTdBeatsHead(t *testing.T) {
	current := &types.Header{Number: big.NewInt(10)}
	localTd := big.NewInt(100)
	sameNumber := syntheticBlock(10, common.Hash{}, common.Hash{})
	higherNumber := syntheticBlock(11, common.Hash{}, common.Hash{})
	lowerNumber := syntheticBlock(9, common.Hash{}, common.Hash{})

	if !externTdBeatsHead(sameNumber, current, big.NewInt(101), localTd) {
		t.Fatal("a higher total difficulty must beat the head")
	}
	if externTdBeatsHead(sameNumber, current, big.NewInt(99), localTd) {
		t.Fatal("a lower total difficulty must not beat the head")
	}
	if externTdBeatsHead(sameNumber, current, big.NewInt(100), localTd) {
		t.Fatal("an equal total difficulty at the same number must not beat the head")
	}
	if !externTdBeatsHead(higherNumber, current, big.NewInt(100), localTd) {
		t.Fatal("an equal total difficulty at a higher number must beat the head")
	}
	if externTdBeatsHead(lowerNumber, current, big.NewInt(100), localTd) {
		t.Fatal("an equal total difficulty at a lower number must not beat the head")
	}
}
