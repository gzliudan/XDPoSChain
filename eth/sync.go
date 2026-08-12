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
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core/txpool"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/eth/downloader"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
)

const (
	forceSyncCycle      = 10 * time.Second // Time interval to force syncs, even if few peers are available
	minDesiredPeerCount = 5                // Amount of peers desired to start syncing

	// This is the target size for the packs of transactions sent by txsyncLoop.
	// A pack can get larger than this if a single transactions exceeds this size.
	txsyncPackSize = 100 * 1024

	// syncStatusLogCycle is the interval at which the current sync status is
	// reported at warn level, so it is visible even when info/debug logs are
	// filtered out.
	syncStatusLogCycle = 10 * time.Minute
)

type txsync struct {
	p   *peer
	txs []*types.Transaction
}

// syncTransactions starts sending all currently pending transactions to the given peer.
func (pm *ProtocolManager) syncTransactions(p *peer) {
	abandoned := func() bool {
		select {
		case <-p.term:
			return true
		case <-pm.quitSync:
			return true
		default:
			return false
		}
	}
	// Bail out if the peer was already dropped or the node began shutting down,
	// so neither a removed peer nor a stopping node triggers a full, potentially
	// expensive pool scan. The sync now runs concurrently with the message
	// loop, so both can happen at any point.
	if abandoned() {
		return
	}

	// Assemble the set of transaction to broadcast or announce to the remote
	// peer. Fun fact, this is quite an expensive operation as it needs to sort
	// the transactions if the sorting is not cached yet. However, with a random
	// order, insertions could overflow the non-executable queues and get dropped.
	//
	// TODO(karalabe): Figure out if we could get away with random order somehow
	pending := pm.txpool.Pending(txpool.PendingFilter{})

	// Abandon the sync if shutdown started or the peer was dropped while the
	// pool was being scanned, so no useless resolve and send work is done.
	if abandoned() {
		return
	}

	// Skip transactions the peer already has. The sync now runs concurrently
	// with the message loop, so transactions received from this peer since
	// registration are marked known and must not be echoed back to it. The
	// known set is shared with the message loop, so a single cardinality read
	// replaces one lock acquisition per pending transaction; a freshly
	// registered peer knows nothing, which is the common case.
	//
	// The read happens after the scan, so anything marked while the pool was
	// being scanned is still filtered. Only a transaction marked during the
	// filtering itself, on a peer that knew nothing when the loop started, can
	// slip through and be echoed back once.
	filter := p.knownTxs.Cardinality() > 0

	// The xdc/165 protocol introduces proper transaction announcements, so instead
	// of dripping transactions across multiple peers, just send the entire list as
	// an announcement and let the remote side decide what they need (likely nothing).
	// Only the hashes are needed, so the pending snapshot is filtered once and
	// the full transactions are never sent. Each hash is checked against the
	// live pool first: queueing marks hashes as known before the announcer
	// drops the vanished ones, so announcing a transaction removed after the
	// snapshot would leave a ghost entry suppressing later announcements of
	// the same transaction.
	if p.version >= xdc165 {
		var hashes []common.Hash
		for _, batch := range pending {
			for _, lazy := range batch {
				if filter && p.knownTxs.Contains(lazy.Hash) {
					continue
				}
				if p.getPooledTx(lazy.Hash) == nil {
					continue
				}
				hashes = append(hashes, lazy.Hash)
			}
		}
		if len(hashes) > 0 {
			p.AsyncSendPooledTransactionHashes(hashes)
		}
		return
	}
	// Out of luck, peer is running legacy protocols, drop the txs over. The
	// full transactions are needed here, so resolve them, skipping the ones
	// the peer already has.
	var txs types.Transactions
	for _, batch := range pending {
		for _, lazy := range batch {
			if filter && p.knownTxs.Contains(lazy.Hash) {
				continue
			}
			if tx := lazy.Resolve(); tx != nil {
				txs = append(txs, tx)
			}
		}
	}
	if len(txs) == 0 {
		return
	}
	// The peer might have been dropped while the pool was being scanned, in
	// which case the sync must not wait for the txsync loop to pick up the
	// batch.
	select {
	case pm.txsyncCh <- &txsync{p, txs}:
	case <-pm.quitSync:
	case <-p.term:
	}
}

// txsyncLoop64 takes care of the initial transaction sync for each new
// connection. When a new peer appears, we relay all currently pending
// transactions. In order to minimise egress bandwidth usage, we send
// the transactions in small packs to one peer at a time.
func (pm *ProtocolManager) txsyncLoop64() {
	var (
		pending = make(map[enode.ID]*txsync)
		sending = false               // whether a send is active
		pack    = new(txsync)         // the pack that is being sent
		done    = make(chan error, 1) // result of the send
	)
	// send packs the unknown transactions of the sync and starts delivering
	// the pack in the background, reporting whether a pack was scheduled. A
	// sync whose transactions all became known while it sat in the queue
	// drains to an empty pack; reporting false lets the caller move on to the
	// next peer instead of waiting for a completion event that never arrives.
	send := func(s *txsync) bool {
		if s.p.version >= xdc165 {
			panic("initial transaction syncer running on xdc/165+")
		}
		// Fill pack with transactions up to the target size, skipping the ones
		// the peer came to know about while the sync was queued. The empty-pack
		// clause keeps a single oversized transaction eligible.
		size := common.StorageSize(0)
		filter := s.p.knownTxs.Cardinality() > 0
		pack.txs = pack.txs[:0]
		for len(s.txs) > 0 && (len(pack.txs) == 0 || size < txsyncPackSize) {
			tx := s.txs[0]
			s.txs = s.txs[1:]
			if filter && s.p.knownTxs.Contains(tx.Hash()) {
				continue
			}
			pack.txs = append(pack.txs, tx)
			size += common.StorageSize(tx.Size())
		}
		// Drop the sync once drained, whether anything was packed or not.
		if len(s.txs) == 0 {
			delete(pending, s.p.ID())
		}
		if len(pack.txs) == 0 {
			return false
		}
		// pack.p is only set for a real send, so the done branch never observes
		// the peer of a sync that filtered down to nothing.
		pack.p = s.p
		// Send the pack in the background.
		s.p.Log().Trace("Sending batch of transactions", "count", len(pack.txs), "bytes", size)
		sending = true
		go func() { done <- pack.p.SendTransactions64(pack.txs) }()
		return true
	}

	// pick chooses the next pending sync.
	pick := func() *txsync {
		if len(pending) == 0 {
			return nil
		}
		n := rand.Intn(len(pending)) + 1
		for _, s := range pending {
			if n--; n == 0 {
				return s
			}
		}
		return nil
	}

	// schedule keeps picking pending syncs until one produces a real pack or
	// the queue is exhausted, so syncs that filtered down to nothing cannot
	// starve the peers parked behind them.
	schedule := func() {
		for {
			s := pick()
			if s == nil || send(s) {
				return
			}
		}
	}

	for {
		select {
		case s := <-pm.txsyncCh:
			pending[s.p.ID()] = s
			if !sending {
				schedule()
			}
		case err := <-done:
			sending = false
			// Stop tracking peers that cause send failures.
			if err != nil {
				pack.p.Log().Debug("Transaction send failed", "err", err)
				delete(pending, pack.p.ID())
			}
			// Schedule the next send.
			schedule()
		case <-pm.quitSync:
			return
		}
	}
}

// syncer is responsible for periodically synchronising with the network, both
// downloading hashes and blocks as well as handling the announcement handler.
func (pm *ProtocolManager) syncer() {
	// Start and ensure cleanup of sync mechanisms
	pm.blockFetcher.Start()
	pm.txFetcher.Start()
	pm.bft.Start()
	defer pm.blockFetcher.Stop()
	defer pm.txFetcher.Stop()
	defer pm.downloader.Terminate()
	defer pm.bft.Stop()

	// Wait for different events to fire synchronisation operations
	forceSync := time.NewTicker(forceSyncCycle)
	defer forceSync.Stop()

	for {
		select {
		case <-pm.newPeerCh:
			// Make sure we have peers to select from, then sync
			if pm.peers.Len() < minDesiredPeerCount {
				break
			}
			go pm.synchronise(pm.peers.BestPeer())

		case <-forceSync.C:
			// Force a sync even if not enough peers are present
			go pm.synchronise(pm.peers.BestPeer())

		case <-pm.noMorePeers:
			return
		}
	}
}

// syncStatusLogger periodically reports the current sync status at warn
// level so that it is always visible in the logs, regardless of whether
// info/debug logs are enabled, and independent of the one-shot start/finish
// logs emitted by the downloader itself.
func (pm *ProtocolManager) syncStatusLogger() {
	ticker := time.NewTicker(syncStatusLogCycle)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.reportSyncStatus()

		case <-pm.quitSync:
			return
		}
	}
}

// reportSyncStatus emits a warn-level periodic sync status line, so that the
// current sync state is always visible in the logs every cycle regardless of
// whether the node is catching up or already in sync.
func (pm *ProtocolManager) reportSyncStatus() {
	var (
		current uint64
		highest uint64
	)
	// Seed current/highest from the downloader while it is actively
	// synchronising (it knows the discovered sync target and, in fast sync,
	// reports the snap block as the current height). Otherwise seed both from
	// the local chain head. computeSyncStatus then folds the live per-peer
	// announced-tip high-water mark into highest in both states, so the
	// reported highest always reflects the freshest known chain tip.
	if pm.downloader.Synchronising() {
		progress := pm.downloader.Progress()
		current, highest = progress.CurrentBlock, progress.HighestBlock
	} else {
		current = pm.blockchain.CurrentBlock().Number.Uint64()
	}
	status := computeSyncStatus(current, highest, pm.peers.HighestTipNumber())
	log.Warn("Block synchronisation status",
		"current", status.current,
		"highest", status.highest,
		"behind", status.behind,
		"peers", pm.peers.Len(),
	)
}

// syncStatus holds the values reported by the periodic sync status heartbeat.
type syncStatus struct {
	current uint64 // Local head, or the fast-sync snap block while bulk syncing
	highest uint64 // Highest known network block (downloader target + announced tips)
	behind  uint64 // Number of blocks behind the highest known network block
}

// computeSyncStatus derives the heartbeat values. It always folds the live
// network high-water mark (the highest block announced by any peer, bounded by
// IsPlausibleAnnouncement) into the reported highest, regardless of whether the
// downloader is bulk-syncing. The reported highest is therefore the maximum of
// the downloader target, the local head and the live announced tip in every
// state, so it reflects the freshest known chain tip.
func computeSyncStatus(current, highest, announcedTip uint64) syncStatus {
	highest = max(current, max(highest, announcedTip))
	behind := uint64(0)
	if highest > current {
		behind = highest - current
	}
	return syncStatus{current: current, highest: highest, behind: behind}
}

// synchronise tries to sync up our local block chain with a remote peer.
func (pm *ProtocolManager) synchronise(peer *peer) {
	// Short circuit if no peers are available
	if peer == nil {
		return
	}
	// Make sure the peer's TD is higher than our own
	currentBlock := pm.blockchain.CurrentBlock()
	td := pm.blockchain.GetTd(currentBlock.Hash(), currentBlock.Number.Uint64())
	pHead, pTd := peer.Head()
	if pTd.Cmp(td) <= 0 {
		return
	}
	// Otherwise try to sync with the downloader
	mode := downloader.FullSync
	if atomic.LoadUint32(&pm.snapSync) == 1 {
		// Fast sync was explicitly requested, and explicitly granted
		mode = downloader.FastSync
	} else if currentBlock.Number.Sign() == 0 && pm.blockchain.CurrentSnapBlock().Number.Sign() > 0 {
		// The database seems empty as the current block is the genesis. Yet the fast
		// block is ahead, so fast sync was enabled for this node at a certain point.
		// The only scenario where this can happen is if the user manually (or via a
		// bad block) rolled back a fast sync node below the sync point. In this case
		// however it's safe to reenable fast sync.
		atomic.StoreUint32(&pm.snapSync, 1)
		mode = downloader.FastSync
	}

	if mode == downloader.FastSync {
		// Make sure the peer's total difficulty we are synchronizing is higher.
		if pm.blockchain.GetTdByHash(pm.blockchain.CurrentSnapBlock().Hash()).Cmp(pTd) >= 0 {
			return
		}
	}

	// Run the sync cycle, and disable fast sync if we've went past the pivot block
	if err := pm.downloader.Synchronise(peer.id, pHead, pTd, mode); err != nil {
		return
	}
	if atomic.LoadUint32(&pm.snapSync) == 1 {
		log.Info("Fast sync complete, auto disabling")
		atomic.StoreUint32(&pm.snapSync, 0)
	}
	atomic.StoreUint32(&pm.acceptTxs, 1) // Mark initial sync done
	//if head := pm.blockchain.CurrentBlock(); head.NumberU64() > 0 {
	//	// We've completed a sync cycle, notify all peers of new state. This path is
	//	// essential in star-topology networks where a gateway node needs to notify
	//	// all its out-of-date peers of the availability of a new block. This failure
	//	// scenario will most often crop up in private and hackathon networks with
	//	// degenerate connectivity, but it should be healthy for the mainnet too to
	//	// more reliably update peers or the local TD state.
	//	go pm.BroadcastBlock(head, false)
	//}
}
