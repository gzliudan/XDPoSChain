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
	"sync/atomic"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/eth/downloader"
	"github.com/XinFinOrg/XDPoSChain/p2p"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
)

// Tests that fast sync is disabled after a successful sync cycle.
func TestFastSyncDisabling100(t *testing.T) { testFastSyncDisabling(t, xdc100) }
func TestFastSyncDisabling164(t *testing.T) { testFastSyncDisabling(t, xdc164) }
func TestFastSyncDisabling165(t *testing.T) { testFastSyncDisabling(t, xdc165) }

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

// Tests that ProtocolManager.synchronise does not panic when the local head
// has no TD entry in the database (legacy XDPoS chaindata predating the TD
// index). The head TD is reconstructed from the canonical chain before the
// threshold check; since the peer is at the same height, the sync then returns
// early without a cycle.
func TestMissingHeadTdFullSync100(t *testing.T) { testMissingHeadTdFullSync(t, xdc100) }
func TestMissingHeadTdFullSync164(t *testing.T) { testMissingHeadTdFullSync(t, xdc164) }
func TestMissingHeadTdFullSync165(t *testing.T) { testMissingHeadTdFullSync(t, xdc165) }

func testMissingHeadTdFullSync(t *testing.T, protocol int) {
	t.Parallel()

	// Create a node whose chain is complete but whose head TD is missing,
	// simulating legacy chaindata, and a passive peer with the same chain.
	pm, _ := newTestProtocolManagerWithMissingHeadTd(t, downloader.FullSync, 512)
	defer pm.Stop()

	peerPM, _ := newTestProtocolManagerPassiveMust(t, downloader.FullSync, 512, nil, nil)
	// The passive peer manager never started, so Stop is unsafe on it; see
	// newTestProtocolManagerPassive. Terminating the downloader suffices.
	defer peerPM.downloader.Terminate()

	io1, io2 := p2p.MsgPipe()
	defer io1.Close()
	defer io2.Close()

	go pm.handle(pm.newPeer(protocol, p2p.NewPeer(enode.ID{}, "peer", nil), io1, pm.txpool.Get))
	go peerPM.handle(peerPM.newPeer(protocol, p2p.NewPeer(enode.ID{}, "victim", nil), io2, peerPM.txpool.Get))

	// Drive the sync loop manually until the head TD has been repaired. The
	// missing TD must not panic the entry checks, and once reconstructed the
	// peer is not ahead of the node, so synchronise returns without a cycle.
	head := pm.blockchain.CurrentBlock()
	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for pm.blockchain.GetTd(head.Hash(), head.Number.Uint64()) == nil {
		select {
		case <-deadline:
			t.Fatalf("head TD not repaired by synchronise")
		case <-ticker.C:
			pm.synchronise(pm.peers.BestPeer())
		}
	}
}

// Tests that ProtocolManager.synchronise does not panic in fast sync mode when
// the local snap head has no TD entry in the database. The entry is repaired
// before the threshold checks, so header insertion succeeds and the fast sync
// completes with the peer still connected.
func TestMissingHeadTdFastSync100(t *testing.T) { testMissingHeadTdFastSync(t, xdc100) }
func TestMissingHeadTdFastSync164(t *testing.T) { testMissingHeadTdFastSync(t, xdc164) }
func TestMissingHeadTdFastSync165(t *testing.T) { testMissingHeadTdFastSync(t, xdc165) }

func testMissingHeadTdFastSync(t *testing.T, protocol int) {
	t.Parallel()

	// Create an empty fast sync node with the genesis TD missing and a passive
	// peer carrying the full chain.
	pm, _ := newTestProtocolManagerWithMissingHeadTd(t, downloader.FastSync, 0)
	defer pm.Stop()

	peerPM, _ := newTestProtocolManagerPassiveMust(t, downloader.FullSync, 512, nil, nil)
	// The passive peer manager never started, so Stop is unsafe on it; see
	// newTestProtocolManagerPassive. Terminating the downloader suffices.
	defer peerPM.downloader.Terminate()

	io1, io2 := p2p.MsgPipe()
	defer io1.Close()
	defer io2.Close()

	go pm.handle(pm.newPeer(protocol, p2p.NewPeer(enode.ID{}, "peer", nil), io1, pm.txpool.Get))
	go peerPM.handle(peerPM.newPeer(protocol, p2p.NewPeer(enode.ID{}, "victim", nil), io2, peerPM.txpool.Get))

	// Drive the sync loop manually until the initial sync completes. The
	// missing TD must not panic the fast sync entry checks; with the entry
	// repaired, the header phase succeeds and the sync finishes.
	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for atomic.LoadUint32(&pm.acceptTxs) == 0 {
		select {
		case <-deadline:
			t.Fatalf("fast sync not completed with repaired head TD")
		case <-ticker.C:
			pm.synchronise(pm.peers.BestPeer())
		}
	}
	if atomic.LoadUint32(&pm.snapSync) == 1 {
		t.Fatalf("fast sync still enabled after successful sync")
	}
	if pm.peers.Len() == 0 {
		t.Fatalf("peer dropped after successful sync with repaired head TD")
	}
}

// Tests that ProtocolManager.synchronise skips the fast sync cycle when the
// snap head TD is missing and cannot be repaired, instead of proceeding into
// the downloader with an unverifiable TD promise. The peer is ahead of the
// node, so the cycle would otherwise start; skipping keeps the peer connected
// and the node unmarked.
func TestMissingSnapTdRepairFailed100(t *testing.T) { testMissingSnapTdRepairFailed(t, xdc100) }
func TestMissingSnapTdRepairFailed164(t *testing.T) { testMissingSnapTdRepairFailed(t, xdc164) }
func TestMissingSnapTdRepairFailed165(t *testing.T) { testMissingSnapTdRepairFailed(t, xdc165) }

func testMissingSnapTdRepairFailed(t *testing.T, protocol int) {
	t.Parallel()

	pm, _ := newTestProtocolManagerWithUnrepairableSnapTd(t)
	defer pm.Stop()

	peerPM, _ := newTestProtocolManagerPassiveMust(t, downloader.FullSync, 576, nil, nil)
	// The passive peer manager never started, so Stop is unsafe on it; see
	// newTestProtocolManagerPassive. Terminating the downloader suffices.
	defer peerPM.downloader.Terminate()

	io1, io2 := p2p.MsgPipe()
	defer io1.Close()
	defer io2.Close()

	go pm.handle(pm.newPeer(protocol, p2p.NewPeer(enode.ID{}, "peer", nil), io1, pm.txpool.Get))
	go peerPM.handle(peerPM.newPeer(protocol, p2p.NewPeer(enode.ID{}, "victim", nil), io2, peerPM.txpool.Get))

	// Wait for the handshake so the peer's advertised head and TD are set
	// before synchronise runs the head TD comparison.
	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		peer := pm.peers.BestPeer()
		if peer != nil {
			if _, td := peer.Head(); td != nil {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("peer handshake not completed")
		case <-ticker.C:
		}
	}

	// synchronise must skip the cycle at the unrepairable snap TD guard. The
	// call is synchronous, so afterwards no downloader cycle may have run:
	// the node stays unmarked and the peer stays connected.
	done := make(chan struct{})
	go func() {
		pm.synchronise(pm.peers.BestPeer())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("synchronise did not return within 15s")
	}
	if atomic.LoadUint32(&pm.acceptTxs) == 1 {
		t.Fatalf("node marked synchronised despite unrepairable snap TD")
	}
	if pm.peers.Len() == 0 {
		t.Fatalf("peer dropped despite unrepairable snap TD")
	}
}
