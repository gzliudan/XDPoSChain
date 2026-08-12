// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core/txpool"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/eth/downloader"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/p2p"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
)

// lockedBuffer is a concurrency-safe log capture buffer. The protocol manager
// under test starts background goroutines (syncer, sync status logger, tx
// broadcast loops) and, in some tests, peer handler goroutines that keep
// emitting trace/debug logs while the test reads and resets the captured output,
// so a plain bytes.Buffer would race under -race and could corrupt the output.
// All access (write/read/reset) is serialized through a mutex.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// stalledDownloaderPeer is a downloader.Peer stub that answers the height probe
// and the common-ancestor search, then blocks all subsequent requests until
// released. It lets a test hold the downloader in a deterministic
// "synchronising with a discovered target" state without depending on a real
// sync still being in flight.
type stalledDownloaderPeer struct {
	id       string
	download *downloader.Downloader
	genesis  *types.Header

	releaseCh   chan struct{}
	releaseOnce sync.Once

	mu             sync.Mutex
	answeredNumber bool // Whether the ancestor-search header request was answered
}

func newStalledDownloaderPeer(dl *downloader.Downloader, id string, genesis *types.Header) *stalledDownloaderPeer {
	return &stalledDownloaderPeer{
		id:        id,
		download:  dl,
		genesis:   genesis,
		releaseCh: make(chan struct{}),
	}
}

// release unblocks all pending requests so the stalled sync can wind down.
func (p *stalledDownloaderPeer) release() {
	p.releaseOnce.Do(func() { close(p.releaseCh) })
}

// Head is unused by the downloader sync path (the head hash/TD are passed into
// Synchronise directly) but must exist to satisfy the downloader.Peer interface.
func (p *stalledDownloaderPeer) Head() (common.Hash, *big.Int) {
	return p.genesis.Hash(), big.NewInt(1)
}

// claimedHead is the header the stub advertises as the remote head, two blocks
// above the local genesis so the ancestor search converges on the genesis.
func (p *stalledDownloaderPeer) claimedHead() *types.Header {
	return &types.Header{
		ParentHash: p.genesis.Hash(),
		Number:     new(big.Int).SetUint64(2),
		Difficulty: big.NewInt(1),
	}
}

// RequestHeadersByHash answers the height probe with the claimed head header.
func (p *stalledDownloaderPeer) RequestHeadersByHash(h common.Hash, amount int, skip int, reverse bool) error {
	return p.download.DeliverHeaders(p.id, []*types.Header{p.claimedHead()})
}

// RequestHeadersByNumber answers the ancestor search (the first call) with the
// genesis and the claimed head so the search converges, then blocks all
// subsequent bulk-download requests until released.
func (p *stalledDownloaderPeer) RequestHeadersByNumber(from uint64, amount int, skip int, reverse bool) error {
	p.mu.Lock()
	answer := !p.answeredNumber
	p.answeredNumber = true
	p.mu.Unlock()
	if !answer {
		<-p.releaseCh
		return nil
	}
	// Reply with the genesis at number 0 plus synthetic headers matching the
	// span request, so the downloader finds the genesis as the common ancestor.
	step := uint64(skip + 1)
	headers := make([]*types.Header, 0, amount)
	for i := 0; i < amount; i++ {
		n := from + uint64(i)*step
		switch {
		case n == 0:
			headers = append(headers, p.genesis)
		case n == 2:
			headers = append(headers, p.claimedHead())
		default:
			headers = append(headers, &types.Header{Number: new(big.Int).SetUint64(n)})
		}
	}
	return p.download.DeliverHeaders(p.id, headers)
}

// RequestBodies blocks until released (the sync is paused during body download).
func (p *stalledDownloaderPeer) RequestBodies([]common.Hash) error { <-p.releaseCh; return nil }

// RequestReceipts blocks until released.
func (p *stalledDownloaderPeer) RequestReceipts([]common.Hash) error { <-p.releaseCh; return nil }

// RequestNodeData blocks until released.
func (p *stalledDownloaderPeer) RequestNodeData([]common.Hash) error { <-p.releaseCh; return nil }

// Tests that fast sync is disabled after a successful sync cycle.
func TestFastSyncDisabling100(t *testing.T) { testFastSyncDisabling(t, xdc100) }
func TestFastSyncDisabling164(t *testing.T) { testFastSyncDisabling(t, xdc164) }
func TestFastSyncDisabling165(t *testing.T) { testFastSyncDisabling(t, xdc165) }

// Tests that the periodic sync status logger emits a status line on every cycle,
// reporting the live network high-water mark from the peers' announced tips,
// regardless of whether the node is catching up or already in sync.
func TestSyncStatusLogger(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	defer pm.Stop()

	// Capture the warn-level logs emitted by the sync status logger.
	logBuf := new(lockedBuffer)
	prevLog := log.Root()
	glog := log.NewGlogHandler(log.NewTerminalHandlerWithLevel(logBuf, log.LevelTrace, false))
	glog.Verbosity(log.LevelTrace)
	log.SetDefault(log.NewLogger(glog))
	defer log.SetDefault(prevLog)

	// Register a peer and give it a tip above our chain head.
	app, net := p2p.MsgPipe()
	defer app.Close()
	peer := pm.newPeer(xdc100, p2p.NewPeer(enode.ID{1}, "sync-status-peer", nil), net, pm.txpool.Get)
	if err := pm.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	defer pm.peers.Unregister(peer.id)

	current := pm.blockchain.CurrentBlock().Number.Uint64()

	// A peer ahead of us must surface the gap...
	peer.SetTipNumber(current + 5)
	pm.reportSyncStatus()
	got := logBuf.String()
	if count := strings.Count(got, "Block synchronisation status"); count != 1 {
		t.Fatalf("expected exactly one sync status log, got %d, log: %q", count, got)
	}
	if !strings.Contains(got, fmt.Sprintf("highest=%d", current+5)) ||
		!strings.Contains(got, fmt.Sprintf("behind=%d", 5)) || !strings.Contains(got, "peers=1") {
		t.Fatalf("expected live high-water mark and gap, got %q", got)
	}
	// ...and once no peer advertises a higher tip, no gap is reported.
	logBuf.Reset()
	if err := pm.peers.Unregister(peer.id); err != nil {
		t.Fatalf("failed to unregister peer: %v", err)
	}
	pm.reportSyncStatus()
	got = logBuf.String()
	if !strings.Contains(got, "behind=0") {
		t.Fatalf("expected no gap when at the peer's head, got %q", got)
	}
}

// Tests that a real NewBlockHashesMsg announcement updates the peer's tip, which
// the sync status heartbeat then surfaces as the network high-water mark.
func TestAnnouncementUpdatesPeerTip(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 8, nil, nil)
	defer pm.Stop()

	// Create a connected peer and deliver a real block-hash announcement for a
	// block the peer is known to have.
	tp, _ := newTestPeer("announce", xdc165, pm, true)
	defer tp.close()

	block := pm.blockchain.GetBlockByNumber(5)
	if block == nil {
		t.Fatalf("block #5 not found")
	}
	if err := p2p.Send(tp.app, NewBlockHashesMsg, newBlockHashesData{{Hash: block.Hash(), Number: block.NumberU64()}}); err != nil {
		t.Fatalf("failed to send block announcement: %v", err)
	}
	// Wait for the protocol handler to record the announced tip.
	deadline := time.After(5 * time.Second)
	for tp.peer.TipNumber() != block.NumberU64() {
		select {
		case <-deadline:
			t.Fatalf("peer tip not updated by NewBlockHashesMsg, have %d", tp.peer.TipNumber())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The heartbeat must reflect the announced tip.
	if highest := pm.peers.HighestTipNumber(); highest != block.NumberU64() {
		t.Fatalf("expected highest tip %d, got %d", block.NumberU64(), highest)
	}
}

// Tests that a far-future block announcement (which the fetcher would discard)
// does not inflate the peer's recorded tip.
func TestAnnouncementRejectsFarFutureTip(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 8, nil, nil)
	defer pm.Stop()

	tp, _ := newTestPeer("announce", xdc165, pm, true)
	defer tp.close()

	// A plausible announcement within the fetcher's window is recorded as the
	// peer's tip.
	block := pm.blockchain.GetBlockByNumber(5)
	if err := p2p.Send(tp.app, NewBlockHashesMsg, newBlockHashesData{{Hash: block.Hash(), Number: block.NumberU64()}}); err != nil {
		t.Fatalf("failed to send block announcement: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for tp.peer.TipNumber() != block.NumberU64() {
		select {
		case <-deadline:
			t.Fatalf("peer tip not updated by plausible announcement, have %d", tp.peer.TipNumber())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A far-future announcement must be discarded and must not move the tip.
	if err := p2p.Send(tp.app, NewBlockHashesMsg, newBlockHashesData{{Hash: common.Hash{0x01}, Number: ^uint64(0)}}); err != nil {
		t.Fatalf("failed to send far-future announcement: %v", err)
	}
	// Observe the tip for a while to ensure the rejected announcement left it
	// unchanged.
	deadline = time.After(500 * time.Millisecond)
	for {
		if got := tp.peer.TipNumber(); got != block.NumberU64() {
			t.Fatalf("far-future announcement moved the peer tip to %d, want %d", got, block.NumberU64())
		}
		select {
		case <-deadline:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Tests that the NewBlockMsg handler records the peer's tip only for blocks
// within the fetcher's plausibility window, and ignores far-future blocks.
func TestNewBlockMsgUpdatesTip(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 8, nil, nil)
	defer pm.Stop()

	tp, _ := newTestPeer("propagate", xdc165, pm, true)
	defer tp.close()

	// A plausible propagated block records its number as the peer's tip.
	plausible := pm.blockchain.GetBlockByNumber(5)
	if plausible == nil {
		t.Fatalf("block #5 not found")
	}
	if err := p2p.Send(tp.app, NewBlockMsg, []any{plausible, big.NewInt(131136)}); err != nil {
		t.Fatalf("failed to send plausible NewBlockMsg: %v", err)
	}
	deadline := time.After(5 * time.Second)
	for tp.peer.TipNumber() != plausible.NumberU64() {
		select {
		case <-deadline:
			t.Fatalf("peer tip not updated by plausible NewBlockMsg, have %d", tp.peer.TipNumber())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A far-future block (structurally valid but outside the plausibility
	// window) must be ignored and must not move the tip.
	future := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).SetUint64(^uint64(0)),
		Difficulty: big.NewInt(1),
		UncleHash:  types.EmptyUncleHash,
		TxHash:     types.EmptyRootHash,
	}).WithBody(types.Body{})
	if err := p2p.Send(tp.app, NewBlockMsg, []any{future, big.NewInt(131136)}); err != nil {
		t.Fatalf("failed to send far-future NewBlockMsg: %v", err)
	}
	// Observe the tip for a while to ensure the rejected block left it
	// unchanged.
	deadline = time.After(500 * time.Millisecond)
	for {
		if got := tp.peer.TipNumber(); got != plausible.NumberU64() {
			t.Fatalf("far-future NewBlockMsg moved the peer tip to %d, want %d", got, plausible.NumberU64())
		}
		select {
		case <-deadline:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Tests that the sync status heartbeat merges the downloader's discovered
// target with the live announced-tip high-water mark and computes the gap.
func TestComputeSyncStatus(t *testing.T) {
	tests := []struct {
		current, highest, announcedTip uint64
		wantHighest, wantBehind        uint64
	}{
		// An active bulk sync target is reported even without announced tips.
		{current: 0, highest: 100, announcedTip: 0, wantHighest: 100, wantBehind: 100},
		// The announced tip keeps the high-water mark live outside bulk syncs.
		{current: 100, highest: 0, announcedTip: 150, wantHighest: 150, wantBehind: 50},
		// The higher of the two sources wins.
		{current: 100, highest: 120, announcedTip: 130, wantHighest: 130, wantBehind: 30},
		// Fully in sync reports no gap.
		{current: 120, highest: 120, announcedTip: 100, wantHighest: 120, wantBehind: 0},
	}
	for i, tt := range tests {
		got := computeSyncStatus(tt.current, tt.highest, tt.announcedTip)
		if got.current != tt.current || got.highest != tt.wantHighest || got.behind != tt.wantBehind {
			t.Fatalf("case %d: got %+v, want current=%d highest=%d behind=%d", i, got, tt.current, tt.wantHighest, tt.wantBehind)
		}
	}
}

// Tests that the sync status heartbeat reports the downloader's discovered
// target while a bulk sync is actively running, instead of the announced-tip
// high-water mark (which peers may not populate during a bulk sync). The sync
// is paused deterministically at the downloader's bulk-download phase by a stub
// peer, so the test never relies on a real sync still being in flight.
func TestSyncStatusDuringSync(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	defer pm.Stop()

	// Capture the warn-level logs emitted by the sync status logger.
	logBuf := new(lockedBuffer)
	prevLog := log.Root()
	glog := log.NewGlogHandler(log.NewTerminalHandlerWithLevel(logBuf, log.LevelTrace, false))
	glog.Verbosity(log.LevelTrace)
	log.SetDefault(log.NewLogger(glog))
	defer log.SetDefault(prevLog)

	// Register a protocol peer so synchronise() has a best peer to pick, and
	// advertise a head ahead of our own.
	app, net := p2p.MsgPipe()
	defer app.Close()
	peer := pm.newPeer(xdc100, p2p.NewPeer(enode.ID{1}, "sync-status-peer", nil), net, pm.txpool.Get)
	if err := pm.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	defer pm.peers.Unregister(peer.id)

	current := pm.blockchain.CurrentBlock()
	localTD := pm.blockchain.GetTd(current.Hash(), current.Number.Uint64())
	peer.lock.Lock()
	peer.head = current.Hash()
	peer.td = new(big.Int).Add(localTD, big.NewInt(100))
	peer.lock.Unlock()

	// Register a stub downloader peer under the same id. It answers the height
	// probe and the ancestor search, then blocks the bulk download, holding the
	// downloader in a deterministic synchronising state with a known target.
	stub := newStalledDownloaderPeer(pm.downloader, peer.id, pm.blockchain.Genesis().Header())
	if err := pm.downloader.RegisterPeer(peer.id, xdc100, stub); err != nil {
		t.Fatalf("failed to register downloader peer: %v", err)
	}
	defer pm.downloader.UnregisterPeer(peer.id)
	defer stub.release()

	// Kick off a sync in the background; the stub keeps it synchronising.
	go pm.synchronise(pm.peers.BestPeer())

	// Wait until the downloader is actively synchronising with a discovered
	// target (the stub answers the height probe and the ancestor search, so the
	// target is set while the bulk download is paused).
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if pm.downloader.Synchronising() && pm.downloader.Progress().HighestBlock > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("downloader never reached an active sync with a discovered target")
		case <-ticker.C:
		}
	}
	// The sync is now deterministically paused with a known target: the
	// heartbeat must report it rather than the (empty) announced-tip high-water
	// mark.
	target := pm.downloader.Progress().HighestBlock
	logBuf.Reset()
	pm.reportSyncStatus()
	got := logBuf.String()
	if !strings.Contains(got, fmt.Sprintf("highest=%d", target)) {
		t.Fatalf("expected heartbeat to report downloader target %d during an active sync, got %q", target, got)
	}
}

// Tests that fast sync gets disabled as soon as a real block is successfully
// imported into the blockchain.
func testFastSyncDisabling(t *testing.T, protocol int) {
	t.Parallel()

	// Create a pristine protocol manager, check that fast sync is left enabled
	pmEmpty, _ := newTestProtocolManagerMust(t, downloader.FastSync, 0, nil, nil)
	defer pmEmpty.Stop()
	if atomic.LoadUint32(&pmEmpty.snapSync) == 0 {
		t.Fatalf("snap sync disabled on pristine blockchain")
	}
	// Create a full protocol manager, check that snap sync gets disabled
	pmFull, _ := newTestProtocolManagerMust(t, downloader.FastSync, 1024, nil, nil)
	defer pmFull.Stop()
	if atomic.LoadUint32(&pmFull.snapSync) == 1 {
		t.Fatalf("snap sync not disabled on non-empty blockchain")
	}
	// Sync up the two peers
	io1, io2 := p2p.MsgPipe()

	go pmFull.handle(pmFull.newPeer(protocol, p2p.NewPeer(enode.ID{}, "empty", nil), io2, pmFull.txpool.Get))
	go pmEmpty.handle(pmEmpty.newPeer(protocol, p2p.NewPeer(enode.ID{}, "full", nil), io1, pmEmpty.txpool.Get))

	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if atomic.LoadUint32(&pmEmpty.snapSync) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("snap sync not disabled after successful synchronisation")
		case <-ticker.C:
			pmEmpty.synchronise(pmEmpty.peers.BestPeer())
		}
	}
}

// pendingCountingPool wraps testTxPool and records Pending invocations, so tests
// can assert that a sync abandoned before the scan never touches the pool.
type pendingCountingPool struct {
	*testTxPool
	calls atomic.Int32
}

func (p *pendingCountingPool) Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction {
	p.calls.Add(1)
	return p.testTxPool.Pending(filter)
}

// resolveCountingResolver counts lazy resolve lookups, to observe whether a sync
// given up on still resolves transactions. When serve is set, Get returns it,
// so live syncs can be observed resolving a real transaction.
type resolveCountingResolver struct {
	calls atomic.Int32
	serve *types.Transaction
}

func (r *resolveCountingResolver) Get(hash common.Hash) *types.Transaction {
	r.calls.Add(1)
	return r.serve
}

// stalledTxPool blocks Pending until released and then serves a single lazy
// transaction whose resolution is observed via the counting resolver.
type stalledTxPool struct {
	*testTxPool
	blocked  chan struct{}
	entered  chan struct{}
	resolver *resolveCountingResolver
}

func (p *stalledTxPool) Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction {
	close(p.entered)
	<-p.blocked
	return map[common.Address][]*txpool.LazyTransaction{
		common.HexToAddress("0xdead"): {
			{Hash: common.HexToHash("0xfeed"), Pool: p.resolver},
		},
	}
}

// lazyTxPool serves a single lazy transaction whose Tx field is nil, so
// LazyTransaction.Resolve has to go through the counting resolver. Get is
// overridden to report the same transaction, so the peer's announcement loop
// accepts the announced hash instead of dropping it as unknown. testTxPool
// always attaches Tx to its lazy transactions, which short-circuits Resolve
// and would make the resolver counter unreachable.
type lazyTxPool struct {
	*testTxPool
	resolver *resolveCountingResolver // must have serve set
}

func (p *lazyTxPool) Get(hash common.Hash) *types.Transaction {
	return p.resolver.serve
}

func (p *lazyTxPool) Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction {
	return map[common.Address][]*txpool.LazyTransaction{
		common.HexToAddress("0xdead"): {
			{Hash: p.resolver.serve.Hash(), Pool: p.resolver},
		},
	}
}

// ghostTxPool serves a pending snapshot entry for a transaction that is no
// longer in the pool: Pending reports its hash while Get (inherited from
// testTxPool) returns nil, matching a transaction removed between the pool
// scan and the announcement assembly.
type ghostTxPool struct {
	*testTxPool
	hash common.Hash
}

func (p *ghostTxPool) Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction {
	return map[common.Address][]*txpool.LazyTransaction{
		common.HexToAddress("0xdead"): {
			{Hash: p.hash, Pool: p},
		},
	}
}

// TestSyncTransactionsAbortsForStoppedNodeBeforeScan ensures a sync that starts
// after the protocol manager began shutting down does not run a full pending
// scan. quitSync is closed early in Stop, so a sync goroutine scheduled after
// that point must bail out before touching the pool.
func TestSyncTransactionsAbortsForStoppedNodeBeforeScan(t *testing.T) {
	pool := &pendingCountingPool{testTxPool: newTestTxPool()}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	pm.Stop()

	_, net := p2p.MsgPipe()
	peer := pm.newPeer(xdc165, p2p.NewPeer(enode.ID{}, "late", nil), net, pm.txpool.Get)
	defer peer.close()

	pm.syncTransactions(peer)

	if calls := pool.calls.Load(); calls != 0 {
		t.Fatalf("Pending called %d times for a sync started after shutdown", calls)
	}
}

// TestSyncTransactionsAbortsForDroppedPeerBeforeScan ensures a peer dropped
// before the initial transaction sync runs does not trigger a full pending
// scan. The scan is expensive when the pool is contended, so it must not run
// for a peer that is already gone.
func TestSyncTransactionsAbortsForDroppedPeerBeforeScan(t *testing.T) {
	pool := &pendingCountingPool{testTxPool: newTestTxPool()}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	defer pm.Stop()

	_, net := p2p.MsgPipe()
	peer := pm.newPeer(xdc165, p2p.NewPeer(enode.ID{}, "dropped", nil), net, pm.txpool.Get)
	peer.close()

	pm.syncTransactions(peer)

	if calls := pool.calls.Load(); calls != 0 {
		t.Fatalf("Pending called %d times for a peer dropped before the scan", calls)
	}
}

// TestSyncTransactionsFiltersTxsMarkedDuringScan ensures the known-transaction
// filter is decided after the pending scan, not before it: a transaction the
// peer sends us while the pool is being scanned must not be echoed back. This
// pins the ordering the cardinality short-circuit depends on, since a filter
// evaluated before the scan would skip the check for a peer that knew nothing
// when the sync started.
func TestSyncTransactionsFiltersTxsMarkedDuringScan(t *testing.T) {
	pool := &stalledTxPool{
		testTxPool: newTestTxPool(),
		blocked:    make(chan struct{}),
		entered:    make(chan struct{}),
		resolver:   &resolveCountingResolver{},
	}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	defer pm.Stop()

	// The announce loop is intentionally not running, so an attempted echo would
	// block in AsyncSendPooledTransactionHashes until the timeout below trips.
	_, net := p2p.MsgPipe()
	peer := pm.newPeer(xdc165, p2p.NewPeer(enode.ID{}, "marker", nil), net, pm.txpool.Get)
	defer peer.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.syncTransactions(peer)
	}()

	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial transaction sync never entered Pending")
	}
	peer.MarkTransaction(common.HexToHash("0xfeed"))
	close(pool.blocked)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("initial transaction sync echoed a transaction marked during the scan")
	}
}

// TestSyncTransactionsAbortsForPeerDroppedDuringScan ensures a peer dropped
// while the pending scan is in flight does not get its transactions resolved
// and sent. Dropping the peer closes p.term, which the sync must observe right
// after Pending returns, skipping the useless resolve and send work.
func TestSyncTransactionsAbortsForPeerDroppedDuringScan(t *testing.T) {
	pool := &stalledTxPool{
		testTxPool: newTestTxPool(),
		blocked:    make(chan struct{}),
		entered:    make(chan struct{}),
		resolver:   &resolveCountingResolver{},
	}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	defer pm.Stop()

	_, net := p2p.MsgPipe()
	peer := pm.newPeer(xdc165, p2p.NewPeer(enode.ID{}, "dropped", nil), net, pm.txpool.Get)

	done := make(chan struct{})
	go func() {
		pm.syncTransactions(peer)
		close(done)
	}()

	// Wait until the sync goroutine is inside Pending, then drop the peer and
	// unblock the pool. The sync must return without resolving the transaction.
	// Close the peer before unblocking the pool so syncTransactions observes the
	// closed term channel as soon as Pending returns.
	select {
	case <-pool.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial transaction sync never entered Pending")
	}
	peer.close()
	close(pool.blocked)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("initial transaction sync did not return after the peer was dropped")
	}
	if calls := pool.resolver.calls.Load(); calls != 0 {
		t.Fatalf("transaction resolved %d times for a peer dropped during the scan", calls)
	}
}

// txsyncPeer is an unregistered peer wired to a message pipe, used to drive
// txsyncLoop64 directly. The app side of the pipe is closed on test cleanup,
// releasing any pack writer still blocked on delivery.
type txsyncPeer struct {
	p   *peer
	app *p2p.MsgPipeRW
}

func newTxsyncPeer(t *testing.T, pm *ProtocolManager, version int, name string) *txsyncPeer {
	t.Helper()
	app, net := p2p.MsgPipe()
	id := randomPeerID(t)
	p := pm.newPeer(version, p2p.NewPeer(id, name, nil), net, pm.txpool.Get)
	t.Cleanup(func() {
		p.close()
		app.Close()
	})
	return &txsyncPeer{p: p, app: app}
}

// submitTxsync queues an initial sync carrying the given transactions, failing
// the test if the sync loop does not take it.
func submitTxsync(t *testing.T, pm *ProtocolManager, p *peer, txs types.Transactions) {
	t.Helper()
	select {
	case pm.txsyncCh <- &txsync{p: p, txs: txs}:
	case <-time.After(5 * time.Second):
		t.Fatalf("initial sync for peer %v was never accepted", p.ID())
	}
}

// expectTransactionMsg reads one TransactionMsg from the app side of a test
// peer's pipe and fails the test unless it carries exactly the wanted
// transactions.
func expectTransactionMsg(t *testing.T, app *p2p.MsgPipeRW, want types.Transactions) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- p2p.ExpectMsg(app, TransactionMsg, want) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("transaction delivery mismatch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the transaction delivery")
	}
}

// expectNoMsg fails the test if any message arrives on the app side of a test
// peer's pipe within the grace window. The lingering read is released by the
// pipe close registered at peer creation.
func expectNoMsg(t *testing.T, app *p2p.MsgPipeRW, grace time.Duration) {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		_, err := app.ReadMsg()
		errc <- err
	}()
	select {
	case err := <-errc:
		t.Fatalf("unexpected message delivered: %v", err)
	case <-time.After(grace):
	}
}

// TestSyncTransactionsAnnouncesWithoutResolving ensures the xdc/165 initial
// sync announces the lazy hashes straight from the pending snapshot without
// resolving a single transaction from the pool, while the legacy path still
// resolves. The positive control proves the resolver counter actually fires,
// so the zero on the announcement path is meaningful rather than a broken
// counter passing vacuously.
func TestSyncTransactionsAnnouncesWithoutResolving(t *testing.T) {
	tx := newTestTransaction(testBankKey, 0, 100)
	resolver := &resolveCountingResolver{serve: tx}
	pool := &lazyTxPool{testTxPool: newTestTxPool(), resolver: resolver}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	defer pm.Stop()

	// xdc/165 announces the pending hash without touching the pool. The
	// announcement reader is required, because txAnnounce is unbuffered and
	// the sync would block on queueing otherwise.
	app, net := p2p.MsgPipe()
	id := randomPeerID(t)
	announce := pm.newPeer(xdc165, p2p.NewPeer(id, "announce", nil), net, pm.txpool.Get)
	go announce.announceTransactions()
	defer announce.close()
	defer app.Close()

	pm.syncTransactions(announce)

	errc := make(chan error, 1)
	go func() { errc <- p2p.ExpectMsg(app, NewPooledTransactionHashesMsg, []common.Hash{tx.Hash()}) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("initial sync announcement failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("initial sync never announced the pending transaction")
	}
	if calls := resolver.calls.Load(); calls != 0 {
		t.Fatalf("announcement path resolved %d transactions, want 0", calls)
	}

	// Control: the legacy path must resolve, proving the counter is wired to
	// LazyTransaction.Resolve and the zero above is not a dead counter.
	app100, net100 := p2p.MsgPipe()
	id100 := randomPeerID(t)
	legacy := pm.newPeer(xdc100, p2p.NewPeer(id100, "legacy", nil), net100, pm.txpool.Get)
	defer legacy.close()
	defer app100.Close()

	pm.syncTransactions(legacy)
	if calls := resolver.calls.Load(); calls == 0 {
		t.Fatal("legacy sync never resolved the pending transaction")
	}
	// Drain the pack the legacy syncer delivers, so its writer is not left
	// blocked on the pipe until cleanup.
	errc = make(chan error, 1)
	go func() { errc <- p2p.ExpectMsg(app100, TransactionMsg, types.Transactions{tx}) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("legacy initial sync delivery failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legacy sync never delivered the pending transaction")
	}
}

// TestSyncTransactionsSkipsTxsVanishedFromPool ensures the xdc/165 initial
// sync neither announces nor marks known a transaction that was removed from
// the pool between the pending snapshot and the announcement assembly. The
// async announcer drops vanished hashes without sending them, but the queueing
// handoff marks them known first, leaving a ghost entry that would suppress
// later announcements if the transaction re-entered the pool.
func TestSyncTransactionsSkipsTxsVanishedFromPool(t *testing.T) {
	hash := common.HexToHash("0xdeadbeef")
	pool := &ghostTxPool{testTxPool: newTestTxPool(), hash: hash}
	pm, _, err := newTestProtocolManagerWithTxPool(downloader.FullSync, 4, nil, pool)
	if err != nil {
		t.Fatalf("failed to create protocol manager: %v", err)
	}
	defer pm.Stop()

	// The announce loop must run for the guard to be observable: txAnnounce is
	// unbuffered, so only a completed handoff can pollute knownTxs. The
	// announcer drops the hash on its existence check, so nothing may reach
	// the wire either way.
	app, net := p2p.MsgPipe()
	id := randomPeerID(t)
	peer := pm.newPeer(xdc165, p2p.NewPeer(id, "ghost", nil), net, pm.txpool.Get)
	go peer.announceTransactions()
	defer peer.close()
	defer app.Close()

	pm.syncTransactions(peer)

	if peer.knownTxs.Contains(hash) {
		t.Fatal("transaction removed from the pool was marked known")
	}
	expectNoMsg(t, app, time.Second)
}

// TestTxsyncSkipsKnownTxsAtPackTime ensures the legacy syncer filters a peer's
// known transactions when it finally packs the queued sync. The peer can learn
// a transaction while its initial sync waits behind another peer's in-flight
// pack, and the packer must then drop it instead of echoing it back.
func TestTxsyncSkipsKnownTxsAtPackTime(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 4, nil, nil)
	defer pm.Stop()

	// The blocker occupies the single in-flight pack; its delivery blocks on
	// the unread pipe until the test drains it below.
	blocker := newTxsyncPeer(t, pm, xdc100, "blocker")
	blockerTxs := types.Transactions{newTestTransaction(testBankKey, 0, 100)}
	submitTxsync(t, pm, blocker.p, blockerTxs)

	// The target's sync is queued behind the blocker with the transaction
	// still unknown, and the peer learns it while parked.
	target := newTxsyncPeer(t, pm, xdc100, "target")
	targetTxs := types.Transactions{newTestTransaction(testBankKey, 1, 100)}
	submitTxsync(t, pm, target.p, targetTxs)
	target.p.MarkTransaction(targetTxs[0].Hash())

	// Draining the blocker's pack is the only way to release the loop, which
	// must then skip the target's now fully known sync.
	expectTransactionMsg(t, blocker.app, blockerTxs)
	expectNoMsg(t, target.app, time.Second)
}

// TestTxsyncEmptyPackDoesNotStarveOthers ensures syncs that drained to nothing
// while queued are skipped without stalling the loop: the peers parked behind
// them must still be served even though an empty pack never produces the
// completion event a busy loop would wait for.
func TestTxsyncEmptyPackDoesNotStarveOthers(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 4, nil, nil)
	defer pm.Stop()

	blocker := newTxsyncPeer(t, pm, xdc100, "blocker")
	blockerTxs := types.Transactions{newTestTransaction(testBankKey, 0, 100)}
	submitTxsync(t, pm, blocker.p, blockerTxs)

	// Two syncs whose whole payload becomes known while parked, plus one that
	// still has something to deliver.
	var drained []*txsyncPeer
	for i := 0; i < 2; i++ {
		p := newTxsyncPeer(t, pm, xdc100, fmt.Sprintf("drained-%d", i))
		txs := types.Transactions{newTestTransaction(testBankKey, uint64(i+1), 100)}
		submitTxsync(t, pm, p.p, txs)
		p.p.MarkTransaction(txs[0].Hash())
		drained = append(drained, p)
	}
	lucky := newTxsyncPeer(t, pm, xdc100, "lucky")
	luckyTxs := types.Transactions{newTestTransaction(testBankKey, 9, 100)}
	submitTxsync(t, pm, lucky.p, luckyTxs)

	// Release the blocker's pack by draining it; the loop must skip the two
	// drained syncs and deliver the lucky peer's transaction.
	expectTransactionMsg(t, blocker.app, blockerTxs)
	expectTransactionMsg(t, lucky.app, luckyTxs)
	for _, p := range drained {
		expectNoMsg(t, p.app, time.Second)
	}
}
