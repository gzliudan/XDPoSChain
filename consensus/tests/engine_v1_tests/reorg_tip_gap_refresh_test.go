package engine_v1_tests

import (
	"bytes"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/params"
	"github.com/stretchr/testify/require"
)

// masternodeRefreshLog is the line UpdateM1 prints once per run, before it reads
// the state of the head. It is the only trace a refresh leaves that can tell two
// refreshes of the same header apart: the refresh is idempotent - it rewrites
// the in-memory snapshot of that very header - so the snapshot it produces is
// the same whether it ran once or twice, and both call sites only report an
// error. Counting the line is how the test below observes how many refreshes
// ran, and no production seam is added for it.
const masternodeRefreshLog = "It's time to update new set of masternodes for the next epoch..."

// syncBuffer collects the logs written while the chain is being built. The
// import logs from more than one goroutine, and bytes.Buffer is not safe for
// concurrent writers, so this serialises them.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestReorgIntoAGapBlockRefreshesMasternodesOnce pins how many times the
// next-epoch masternode refresh runs for a reorg whose tip is a gap block.
//
// The apply loop of a reorg walks the whole new chain, tip included, and
// writeBlockWithState refreshes the block it has just made canonical with the
// same gap-block predicate, so such a tip used to be refreshed once per site:
// the whole refresh, logs included, reported twice for one header. The tip
// belongs to the caller, the way upstream keeps the tip out of its own apply
// loop, so exactly one refresh may happen.
//
// This package's mock config has epoch 900 and gap 450, so block 450 is the
// only gap block of the first epoch (the v2 switch is at 900, the segment stays
// v1). The canonical gap block is written first, and then a fork of it with one
// unit of difficulty more: a fork of the same height has to win the fork choice
// to be applied through a reorg, an equal difficulty would leave it a side
// chain.
//
// The two windows are asserted separately on purpose. The extension of the
// canonical gap block may only refresh once - the head write owns that block,
// and a refresh lost there is a refresh that never ran - while the reorg may
// only refresh once as well, even though two sites hold the block.
func TestReorgIntoAGapBlockRefreshesMasternodesOnce(t *testing.T) {
	var logBuf syncBuffer
	previous := log.Root()
	handler := log.NewGlogHandler(log.NewTerminalHandlerWithLevel(&logBuf, log.LevelTrace, false))
	handler.Verbosity(log.LevelTrace)
	log.SetDefault(log.NewLogger(handler))
	defer log.SetDefault(previous)

	blockchain, _, parentBlock, signer, signFn := PrepareXDCTestBlockChain(t, GAP-1, params.TestXDPoSMockChainConfig)

	// The fixture refreshes once itself, so every count below is measured from
	// the baseline taken here.
	refreshes := func() int { return strings.Count(logBuf.String(), masternodeRefreshLog) }
	baseline := refreshes()

	// Block GAP as the canonical block of the initial chain: an extension, where
	// the head write is the only site that can refresh it.
	gapBlockA, err := createBlockFromHeader(blockchain, &types.Header{
		Root:       common.HexToHash("35999dded35e8db12de7e6c1471eb9670c162eec616ecebbaf4fddd4676fb930"),
		Number:     big.NewInt(int64(GAP)),
		ParentHash: parentBlock.Hash(),
		Coinbase:   common.HexToAddress("0xaaa0000000000000000000000000000000000450"),
	}, nil, signer, signFn, blockchain.Config())
	require.NoError(t, err)
	require.NoError(t, blockchain.InsertBlock(gapBlockA))
	require.Equal(t, gapBlockA.Hash(), blockchain.CurrentBlock().Hash(),
		"the gap block must be canonical before the reorg")

	extensionRefreshes := refreshes() - baseline
	require.Equal(t, 1, extensionRefreshes,
		"writing the canonical gap block must refresh it once, its head write owns that block")

	// The fork of the same height, one unit of difficulty ahead, so that the
	// reorg lands on a tip that is the gap block itself.
	gapBlockB, err := createBlockFromHeader(blockchain, &types.Header{
		Root:       common.HexToHash("35999dded35e8db12de7e6c1471eb9670c162eec616ecebbaf4fddd4676fb930"),
		Number:     big.NewInt(int64(GAP)),
		ParentHash: parentBlock.Hash(),
		Coinbase:   common.HexToAddress("0xbbb0000000000000000000000000000000000450"),
		Difficulty: big.NewInt(2),
	}, nil, signer, signFn, blockchain.Config())
	require.NoError(t, err)
	require.NoError(t, blockchain.InsertBlock(gapBlockB))

	head := blockchain.CurrentBlock()
	require.Equal(t, gapBlockB.Hash(), head.Hash(), "the fork must win the fork choice")
	require.Equal(t, uint64(GAP), head.Number.Uint64(),
		"the reorg tip must be the gap block itself")

	reorgRefreshes := refreshes() - baseline - extensionRefreshes
	require.Equal(t, 1, reorgRefreshes,
		"the reorg must refresh its tip once, not once per site that holds a gap block")

	// The refresh above must have reached that block and produced its set, not
	// merely logged once.
	signers, err := GetSnapshotSigner(blockchain, gapBlockB.Header())
	require.NoError(t, err)
	require.NotEmpty(t, signers,
		"the reorg tip must carry the masternode set the refresh wrote for it")
}
