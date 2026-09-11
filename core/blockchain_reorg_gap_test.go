package core

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/state"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/params"
)

// TestReorgGapBlockUnreadableStateReturnsError pins the reorg half of C1: when a
// reorg's new chain carries a gap block whose committed state is no longer in
// the database, the next-epoch refresh must fail with an error instead of
// killing the process through log.Crit. The refresh runs before any side effect,
// so the failed reorg leaves the head exactly where it was.
//
// The assertions below are themselves the proof that the process survived: a
// log.Crit would call os.Exit and the test binary would never reach them.
func TestReorgGapBlockUnreadableStateReturnsError(t *testing.T) {
	// The test only needs reorg to walk a single step up, so a gap block at
	// Epoch-Gap (450 for the mock config) is enough; blocks below its parent are
	// never read.
	config := params.TestXDPoSMockChainConfig
	gapNumber := config.XDPoS.Epoch - config.XDPoS.Gap
	if !config.XDPoS.IsGapBlock(gapNumber) {
		t.Fatalf("test setup expects %d to be a gap block", gapNumber)
	}

	chain := newGapChainFixture(t, nil)
	db := chain.ChainDb()

	// Parent of the gap block, made the current head so the head assertion below
	// has a meaningful baseline. Its state root is the genesis root, which is
	// readable; only the gap block's state is meant to be missing.
	parent := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(gapNumber - 1),
		ParentHash: chain.Genesis().Hash(),
		Root:       chain.Genesis().Root(),
		Difficulty: big.NewInt(1),
		GasLimit:   10_000_000,
	})
	chain.writeHeadBlock(parent, true)
	if head := chain.CurrentBlock(); head.Hash() != parent.Hash() {
		t.Fatalf("test setup expects the parent to be the head, got %d", head.Number.Uint64())
	}

	// The gap block sits right above the head and its state root is not in the
	// database, so UpdateM1At fails while opening its state.
	unreadableRoot := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000dead")
	if chain.HasState(unreadableRoot) {
		t.Fatal("test setup expects the chosen state root to be absent from the database")
	}
	gapBlock := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(gapNumber),
		ParentHash: parent.Hash(),
		Root:       unreadableRoot,
		Difficulty: big.NewInt(1),
		GasLimit:   10_000_000,
	})
	// The gap block must be retrievable, otherwise reorg bails out with
	// errInvalidNewChain before reaching the refresh.
	rawdb.WriteBlock(db, gapBlock)

	headBefore := chain.CurrentBlock().Hash()

	err := chain.reorg(parent.Header(), gapBlock.Header())
	if err == nil {
		t.Fatal("expected the reorg to fail when the gap block state cannot be read")
	}
	if !strings.Contains(err.Error(), "failed to update masternodes during reorg") {
		t.Fatalf("reorg failed for the wrong reason: %v", err)
	}
	if got := chain.CurrentBlock().Hash(); got != headBefore {
		t.Fatalf("head moved despite the failed refresh: got %s, want %s", got.Hex(), headBefore.Hex())
	}
}

// gapStateWithCandidate commits a state on top of base that carries one
// masternode candidate, so the next-epoch derivation of a gap block really
// yields a set and the refresh writes a snapshot. The slots are the ones the
// voting contract exposes and DeriveNextEpochMasternodes reads.
func gapStateWithCandidate(t *testing.T, chain *BlockChain, base common.Hash, number uint64) common.Hash {
	t.Helper()
	statedb, err := state.New(base, chain.stateCache)
	if err != nil {
		t.Fatalf("test setup cannot open the base state: %v", err)
	}
	contract := common.MasternodeVotingSMCBinary
	candidate := common.Address{0x0a}
	candidatesSlot := common.BigToHash(big.NewInt(8)) // slotValidatorMapping["candidates"]
	statedb.SetState(contract, candidatesSlot, common.BigToHash(big.NewInt(1)))
	statedb.SetState(contract, state.GetLocDynamicArrAtElement(candidatesSlot, 0, 1), candidate.Hash())
	// slotValidatorMapping["validatorsState"][_candidate].cap
	capSlot := new(big.Int).Add(state.GetLocMappingAtKey(candidate.Hash(), 7), big.NewInt(1))
	statedb.SetState(contract, common.BigToHash(capSlot), common.BigToHash(big.NewInt(100)))

	root, err := statedb.Commit(number, false)
	if err != nil {
		t.Fatalf("test setup cannot commit the candidate state: %v", err)
	}
	chain.triedb.Reference(root, common.Hash{})
	return root
}

// TestReorgGapBlockStoredSnapshotSkipsRefresh pins C1: a gap block of the new
// chain whose snapshot is already stored must not be re-derived, because the
// stored snapshot is the set that derivation would produce from the same state.
// Without the probe the reorg fails as soon as that state is no longer readable,
// which is exactly what happens on a pruned node revisiting a branch it already
// followed.
func TestReorgGapBlockStoredSnapshotSkipsRefresh(t *testing.T) {
	config := params.TestXDPoSMockChainConfig
	// A gap block of the v2 era: the probe only applies to the v2 schedule.
	gapNumber := 2*config.XDPoS.Epoch - config.XDPoS.Gap
	if !config.XDPoS.IsGapBlock(gapNumber) {
		t.Fatalf("test setup expects %d to be a gap block", gapNumber)
	}
	if config.XDPoS.BlockConsensusVersion(new(big.Int).SetUint64(gapNumber)) != params.ConsensusEngineVersion2 {
		t.Fatalf("test setup expects %d to be a v2-era block", gapNumber)
	}

	chain := newGapChainFixture(t, nil)
	db := chain.ChainDb()

	parent := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(gapNumber - 1),
		ParentHash: chain.Genesis().Hash(),
		Root:       chain.Genesis().Root(),
		Difficulty: big.NewInt(1),
		GasLimit:   10_000_000,
	})
	chain.writeHeadBlock(parent, true)

	// The gap block's own state is gone, so only a stored snapshot can satisfy
	// the refresh.
	unreadableRoot := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000dead")
	if chain.HasState(unreadableRoot) {
		t.Fatal("test setup expects the chosen state root to be absent from the database")
	}
	gapBlock := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(gapNumber),
		ParentHash: parent.Hash(),
		Root:       unreadableRoot,
		Difficulty: big.NewInt(1),
		GasLimit:   10_000_000,
	})
	rawdb.WriteBlock(db, gapBlock)

	if err := rawdb.WriteXdposV2Snapshot(db, gapBlock.Hash(), []byte(`{"number":1350}`)); err != nil {
		t.Fatalf("test setup cannot store the gap block snapshot: %v", err)
	}

	if err := chain.reorg(parent.Header(), gapBlock.Header()); err != nil {
		t.Fatalf("a gap block with a stored snapshot must not fail the reorg on its missing state: %v", err)
	}
	if got := chain.CurrentBlock().Hash(); got != gapBlock.Hash() {
		t.Fatalf("the reorg did not advance the head to the gap block: got %s, want %s", got.Hex(), gapBlock.Hash().Hex())
	}
}

// TestReorgGapBlockFailedRefreshRollsBackSnapshots pins E2: the refresh loop
// writes the next-epoch snapshots of the new chain before any side effect, so an
// attempt that fails later leaves snapshots behind for a branch that never
// became canonical. SetHead's cleanup only walks the canonical chain and cannot
// find them, so the failed reorg has to roll back the ones it wrote.
func TestReorgGapBlockFailedRefreshRollsBackSnapshots(t *testing.T) {
	// A tight schedule keeps the walk short. The gap blocks stay above the mock
	// config's v2 switch block, where the refresh writes a snapshot to disk.
	xdpos := *params.TestXDPoSMockChainConfig.XDPoS
	xdpos.Epoch = 10
	xdpos.Gap = 5
	config := params.TestXDPoSMockChainConfig.Clone()
	config.XDPoS = &xdpos

	chain := newGapChainFixtureWithConfig(t, config, nil)
	db := chain.ChainDb()
	genesis := chain.Genesis()

	const (
		headNum   = uint64(904) // the head the branching starts from
		firstGap  = uint64(905) // its refresh succeeds and writes a snapshot
		secondGap = uint64(915) // its refresh fails
	)
	if !config.XDPoS.IsGapBlock(firstGap) || !config.XDPoS.IsGapBlock(secondGap) {
		t.Fatalf("test setup expects %d and %d to be gap blocks", firstGap, secondGap)
	}

	head := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(headNum),
		ParentHash: genesis.Hash(),
		Root:       genesis.Root(),
		Difficulty: big.NewInt(1),
		GasLimit:   10_000_000,
	})
	chain.writeHeadBlock(head, true)

	unreadableRoot := common.HexToHash("0x000000000000000000000000000000000000000000000000000000000000dead")
	if chain.HasState(unreadableRoot) {
		t.Fatal("test setup expects the chosen state root to be absent from the database")
	}

	// The new chain head -> 5 -> ... -> 15: the refresh visits 5 first (and must
	// write its snapshot) and 15 last (and must fail on its missing state).
	var firstGapBlock, newHeadBlock *types.Block
	parentHash := head.Hash()
	for n := firstGap; n <= secondGap; n++ {
		root := genesis.Root()
		switch n {
		case firstGap:
			root = gapStateWithCandidate(t, chain, genesis.Root(), n)
		case secondGap:
			root = unreadableRoot
		}
		block := types.NewBlockWithHeader(&types.Header{
			Number:     new(big.Int).SetUint64(n),
			ParentHash: parentHash,
			Root:       root,
			Difficulty: big.NewInt(1),
			GasLimit:   10_000_000,
		})
		rawdb.WriteBlock(db, block)
		switch n {
		case firstGap:
			firstGapBlock = block
		case secondGap:
			newHeadBlock = block
		}
		parentHash = block.Hash()
	}
	if firstGapBlock == nil || newHeadBlock == nil {
		t.Fatal("test setup did not build the new chain")
	}
	if stored, err := rawdb.HasXdposV2Snapshot(db, firstGapBlock.Hash()); err != nil {
		t.Fatalf("cannot probe the snapshot store: %v", err)
	} else if stored {
		t.Fatal("test setup expects the first gap block to have no snapshot yet")
	}

	err := chain.reorg(head.Header(), newHeadBlock.Header())
	if err == nil {
		t.Fatal("expected the reorg to fail when the last gap block state cannot be read")
	}
	if !strings.Contains(err.Error(), "failed to update masternodes during reorg") {
		t.Fatalf("the reorg failed for the wrong reason: %v", err)
	}
	// The failure has to name the last gap block: that is what proves the first
	// one was refreshed successfully, and thus that its snapshot was written and
	// had to be rolled back below.
	if !strings.Contains(err.Error(), fmt.Sprintf("number %d", secondGap)) {
		t.Fatalf("the reorg must fail on gap block %d, got %v", secondGap, err)
	}
	if stored, err := rawdb.HasXdposV2Snapshot(db, firstGapBlock.Hash()); err != nil {
		t.Fatalf("cannot probe the snapshot store: %v", err)
	} else if stored {
		t.Fatal("the snapshot written by the failed reorg was not rolled back")
	}
}
