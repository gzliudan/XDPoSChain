package engine_v2_tests

import (
	"math/big"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus/XDPoS"
	"github.com/XinFinOrg/XDPoSChain/consensus/XDPoS/engines/engine_v2"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/params"
	"github.com/stretchr/testify/assert"
)

// TestUpdateM1AtRefreshesSnapshotBelowHead pins the parameterised entry point:
// the next-epoch masternode set must be derivable from the state of a gap block
// that is no longer the chain head, the derived set must match
// engine_v2.BuildSnapshotFromState (the derivation used by fast sync and the
// startup repair), and the head must be left untouched.
func TestUpdateM1AtRefreshesSnapshotBelowHead(t *testing.T) {
	skipLongInShortMode(t)
	config := params.TestXDPoSMockChainConfig
	blockchain, _, currentBlock, _, _, _ := PrepareXDCTestBlockChainForV2Engine(t, 1800, config, nil)
	engine := blockchain.Engine().(*XDPoS.XDPoS)

	gapNumber := uint64(1350)
	gapHeader := blockchain.GetHeaderByNumber(gapNumber)
	if gapHeader == nil {
		t.Fatalf("canonical gap header %d not found", gapNumber)
	}
	if head := blockchain.CurrentBlock(); head.Number.Uint64() == gapNumber {
		t.Fatalf("test setup expects the gap block below the head, head is %d", head.Number.Uint64())
	}

	// The expected set is the one derived from the gap block's own state.
	statedb, err := blockchain.StateAt(gapHeader.Root)
	if err != nil {
		t.Fatal(err)
	}
	want, err := engine_v2.BuildSnapshotFromState(statedb, gapNumber, gapHeader.Hash())
	if err != nil {
		t.Fatal(err)
	}

	headBefore := blockchain.CurrentBlock().Hash()
	if err := blockchain.UpdateM1At(gapHeader); err != nil {
		t.Fatalf("UpdateM1At(gap %d) failed: %v", gapNumber, err)
	}
	assert.Equal(t, headBefore, blockchain.CurrentBlock().Hash(), "refreshing the snapshot must not move the head")

	// Read it back the way the engine does: the head at 1800 maps to the gap
	// block at 1350.
	got, err := engine.EngineV2.GetSnapshot(blockchain, currentBlock.Header())
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, gapNumber, got.Number)
	assert.Equal(t, want.NextEpochCandidates, got.NextEpochCandidates)
}

// TestUpdateM1AtRejectsNonGapBlock guards that the header really is the one
// evaluated: the v2 engine refuses to take a snapshot on a non gap block, and a
// rejected refresh must not move the head either.
func TestUpdateM1AtRejectsNonGapBlock(t *testing.T) {
	skipLongInShortMode(t)
	config := params.TestXDPoSMockChainConfig
	blockchain, _, _, _, _, _ := PrepareXDCTestBlockChainForV2Engine(t, 1400, config, nil)

	head := blockchain.CurrentHeader()
	if head == nil {
		t.Fatal("no current header")
	}
	if head.Number.Uint64()%config.XDPoS.Epoch == config.XDPoS.Epoch-config.XDPoS.Gap {
		t.Fatalf("test setup expects a non gap head, got %d", head.Number.Uint64())
	}
	headBefore := blockchain.CurrentBlock().Hash()

	err := blockchain.UpdateM1At(head)
	if err == nil {
		t.Fatal("expected the v2 engine to reject a non gap block")
	}
	// Any failure would satisfy the nil check above, including one that never
	// looked at the header argument. Pin the rejection itself.
	assert.ErrorContains(t, err, "not gap block")
	assert.Equal(t, headBefore, blockchain.CurrentBlock().Hash(), "a rejected refresh must not move the head")
}

// TestUpdateM1AtFailsLoudlyWhenStateUnavailable pins the invariant that the
// masternode set is never derived from anything but the given block. The head is
// deliberately a gap block whose own state is readable, so an implementation
// that ignored the given header and used the head would succeed and persist a
// snapshot: the refresh must instead fail and store nothing.
//
// The assertion pins the "state of this block cannot be opened" rejection: a root
// that is not in the database must fail the refresh rather than fall back to the
// head. The other failure path, a state that opens but whose voting contract
// storage cannot be read, is covered by
// engine_v2.TestBuildSnapshotFromStateReturnsStateReadError, which exercises the
// same derivation UpdateM1At now delegates to.
func TestUpdateM1AtFailsLoudlyWhenStateUnavailable(t *testing.T) {
	skipLongInShortMode(t)
	config := params.TestXDPoSMockChainConfig
	blockchain, _, _, _, _, _ := PrepareXDCTestBlockChainForV2Engine(t, int(config.XDPoS.Epoch+config.XDPoS.Gap), config, nil)

	head := blockchain.CurrentHeader()
	if head == nil {
		t.Fatal("no current header")
	}
	if head.Number.Uint64()%config.XDPoS.Epoch != config.XDPoS.Epoch-config.XDPoS.Gap {
		t.Fatalf("test setup expects the head to be a gap block, got %d", head.Number.Uint64())
	}

	// Same height as the head, but a state root that is not in the database.
	unreadable := &types.Header{
		Number: new(big.Int).Set(head.Number),
		Root:   common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000dead"),
	}
	headBefore := blockchain.CurrentBlock().Hash()

	err := blockchain.UpdateM1At(unreadable)
	if err == nil {
		t.Fatal("expected the refresh to fail when the block state cannot be read")
	}
	assert.ErrorContains(t, err, "failed to open state of block")
	assert.Equal(t, headBefore, blockchain.CurrentBlock().Hash(), "a failed refresh must not move the head")

	stored, err := rawdb.HasXdposV2Snapshot(blockchain.ChainDb(), unreadable.Hash())
	if err != nil {
		t.Fatalf("cannot probe the snapshot store: %v", err)
	}
	assert.False(t, stored, "a failed refresh must not persist a snapshot")
}

// TestInsertGapBlockStoresSnapshotOnInsertion covers the insertion path instead
// of the entry point: the chain is built up to the gap block's parent only, so
// the refresh under test is the one writeBlockWithState performs on the
// extension path, and by the time the block is announced its next-epoch
// snapshot must already be in the database.
//
// The assertion deliberately stops there. It cannot tell whether the refresh ran
// before or after writeHeadBlock: both finish before insertChain returns, and
// the ChainHeadEvent that ends this test is emitted by PostChainEvents after
// that (writeHeadBlock itself sends nothing). The ordering is pinned from the
// entry point instead: TestUpdateM1AtRejectsNonGapBlock and
// TestUpdateM1AtFailsLoudlyWhenStateUnavailable both assert that a rejected
// refresh leaves the head where it was. A failure on the extension path is
// reported as an insertion error and is pinned in core by
// TestInsertGapBlockRefreshFailureReturnsError and
// TestInsertGapBlockRefreshFailureDoesNotReExecute.
func TestInsertGapBlockStoresSnapshotOnInsertion(t *testing.T) {
	skipLongInShortMode(t)
	config := legacyExecutionConfigForV2Tests(params.TestXDPoSMockChainConfig)
	gapNumber := int(config.XDPoS.Epoch + config.XDPoS.Epoch - config.XDPoS.Gap)
	if gapNumber%int(config.XDPoS.Epoch) != int(config.XDPoS.Epoch-config.XDPoS.Gap) {
		t.Fatalf("test setup expects %d to be a gap block", gapNumber)
	}
	blockchain, _, currentBlock, signer, signFn, _ := PrepareXDCTestBlockChainForV2Engine(t, gapNumber-1, config, nil)

	// Build the gap block the same way the helper builds its predecessors.
	roundNumber := int64(gapNumber) - config.XDPoS.V2.SwitchBlock.Int64()
	gapBlock := CreateBlock(blockchain, config, currentBlock, gapNumber, roundNumber, signer.Hex(), signer, signFn, nil, nil, "")
	// The gap block must extend the head, so the refresh exercised below is the
	// extension path of writeBlockWithState and not the reorg path.
	if head := blockchain.CurrentBlock(); head.Hash() != gapBlock.ParentHash() {
		t.Fatalf("test setup expects the gap block to extend the head, head is %d", head.Number.Uint64())
	}

	stored, err := rawdb.HasXdposV2Snapshot(blockchain.ChainDb(), gapBlock.Hash())
	if err != nil {
		t.Fatalf("cannot probe the snapshot store: %v", err)
	}
	assert.False(t, stored, "the gap block has not been inserted yet, so it cannot have a snapshot")

	headEvents := make(chan core.ChainHeadEvent, 1)
	sub := blockchain.SubscribeChainHeadEvent(headEvents)
	defer sub.Unsubscribe()

	if err := blockchain.InsertBlock(gapBlock); err != nil {
		t.Fatalf("inserting the gap block failed: %v", err)
	}

	select {
	case ev := <-headEvents:
		assert.Equal(t, gapBlock.Hash(), ev.Block.Hash())
		stored, err := rawdb.HasXdposV2Snapshot(blockchain.ChainDb(), gapBlock.Hash())
		if err != nil {
			t.Fatalf("cannot probe the snapshot store: %v", err)
		}
		assert.True(t, stored, "the next-epoch snapshot must be stored before the head event is emitted")
	case <-time.After(10 * time.Second):
		t.Fatal("no chain head event was emitted for the inserted gap block")
	}

	assert.Equal(t, gapBlock.Hash(), blockchain.CurrentBlock().Hash(), "the head must advance to the inserted gap block")
}
