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

// This file contains some shares testing functionality, common to  multiple
// different files and modules being tested.

package eth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus/ethash"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/forkid"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/txpool"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/core/vm"
	"github.com/XinFinOrg/XDPoSChain/crypto"
	"github.com/XinFinOrg/XDPoSChain/eth/downloader"
	"github.com/XinFinOrg/XDPoSChain/eth/ethconfig"
	"github.com/XinFinOrg/XDPoSChain/ethdb"
	"github.com/XinFinOrg/XDPoSChain/event"
	"github.com/XinFinOrg/XDPoSChain/p2p"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
	"github.com/XinFinOrg/XDPoSChain/params"
	"github.com/holiman/uint256"
)

var (
	testBankKey, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	testBank       = crypto.PubkeyToAddress(testBankKey.PublicKey)
)

// newTestProtocolManager creates a new protocol manager for testing purposes,
// with the given number of blocks already known, and potential notification
// channels for different events.
func newTestProtocolManager(mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database, error) {
	pm, db, err := buildTestProtocolManager(mode, blocks, generator, newtx)
	if err != nil {
		return nil, nil, err
	}
	pm.Start(1000)
	return pm, db, nil
}

// newTestProtocolManagerPassive is like newTestProtocolManager, except that the
// background loops are not started. The manager only serves protocol requests
// and never initiates a synchronization of its own, which makes it usable as
// the remote side of a synchronization test.
//
// The returned manager must not be torn down with ProtocolManager.Stop: its
// event subscriptions are nil without Start and would crash the teardown.
// Terminating the downloader is sufficient cleanup.
func newTestProtocolManagerPassive(mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database, error) {
	pm, db, err := buildTestProtocolManager(mode, blocks, generator, newtx)
	if err != nil {
		return nil, nil, err
	}
	// The background loops are intentionally left unstarted, but the peer
	// limit must be set so that handle accepts incoming connections.
	pm.maxPeers = 1000
	return pm, db, nil
}

// buildTestProtocolManager creates a new protocol manager for testing purposes,
// with the given number of blocks already known. The background loops are left
// unstarted, so the caller can choose between newTestProtocolManager and
// newTestProtocolManagerPassive.
func buildTestProtocolManager(mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database, error) {
	var (
		evmux  = new(event.TypeMux)
		engine = ethash.NewFaker()
		db     = rawdb.NewMemoryDatabase()
		gspec  = &core.Genesis{
			Alloc:  types.GenesisAlloc{testBank: {Balance: new(big.Int).SetUint64(10000000000000000000)}},
			Config: params.TestChainConfig,
		}
		genesis       = gspec.MustCommit(db)
		blockchain, _ = core.NewBlockChain(db, nil, gspec, engine, vm.Config{})
	)
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, blocks, generator)
	if _, err := blockchain.InsertChain(chain); err != nil {
		panic(err)
	}

	txpool := newTestTxPool()
	txpool.added = newtx
	pm, err := NewProtocolManager(gspec.Config, mode, ethconfig.Defaults.NetworkId, evmux, &testTxPool{added: newtx, pool: make(map[common.Hash]*types.Transaction)}, engine, blockchain, db)
	if err != nil {
		return nil, nil, err
	}
	return pm, db, nil
}

// newTestProtocolManagerMust creates a new protocol manager for testing purposes,
// with the given number of blocks already known, and potential notification
// channels for different events. In case of an error, the constructor force-
// fails the test.
func newTestProtocolManagerMust(t *testing.T, mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database) {
	pm, db, err := newTestProtocolManager(mode, blocks, generator, newtx)
	if err != nil {
		t.Fatalf("Failed to create protocol manager: %v", err)
	}
	return pm, db
}

// newTestProtocolManagerPassiveMust creates a new passive protocol manager for
// testing purposes, with the given number of blocks already known, and
// potential notification channels for different events. In case of an error,
// the constructor force-fails the test.
func newTestProtocolManagerPassiveMust(t *testing.T, mode downloader.SyncMode, blocks int, generator func(int, *core.BlockGen), newtx chan<- []*types.Transaction) (*ProtocolManager, ethdb.Database) {
	pm, db, err := newTestProtocolManagerPassive(mode, blocks, generator, newtx)
	if err != nil {
		t.Fatalf("Failed to create protocol manager: %v", err)
	}
	return pm, db
}

// newTestProtocolManagerWithMissingHeadTd creates a protocol manager whose
// current head has no TD entry in the database, simulating legacy XDPoS
// chaindata that predates the TD index. The chain is imported normally, the
// head TD is then deleted, and a fresh blockchain is loaded from the mutated
// database so the in-memory TD cache cannot mask the missing entry.
func newTestProtocolManagerWithMissingHeadTd(t *testing.T, mode downloader.SyncMode, blocks int) (*ProtocolManager, ethdb.Database) {
	var (
		evmux  = new(event.TypeMux)
		engine = ethash.NewFaker()
		db     = rawdb.NewMemoryDatabase()
		gspec  = &core.Genesis{
			Alloc:  types.GenesisAlloc{testBank: {Balance: new(big.Int).SetUint64(10000000000000000000)}},
			Config: params.TestChainConfig,
		}
	)
	genesis := gspec.MustCommit(db)
	blockchain, err := core.NewBlockChain(db, nil, gspec, engine, vm.Config{})
	if err != nil {
		t.Fatalf("Failed to create test blockchain: %v", err)
	}
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, blocks, nil)
	if _, err := blockchain.InsertChain(chain); err != nil {
		t.Fatalf("Failed to insert test chain: %v", err)
	}
	head := blockchain.CurrentBlock()
	rawdb.DeleteTd(db, head.Hash(), head.Number.Uint64())

	// Reload a fresh blockchain from the mutated database: the previous
	// instance cached the head TD in memory, which would mask the deletion.
	blockchain, err = core.NewBlockChain(db, nil, gspec, engine, vm.Config{})
	if err != nil {
		t.Fatalf("Failed to reload test blockchain: %v", err)
	}
	pm, err := NewProtocolManager(gspec.Config, mode, ethconfig.Defaults.NetworkId, evmux, &testTxPool{pool: make(map[common.Hash]*types.Transaction)}, engine, blockchain, db)
	if err != nil {
		t.Fatalf("Failed to create protocol manager: %v", err)
	}
	pm.Start(1000)
	return pm, db
}

// newTestProtocolManagerWithUnrepairableSnapTd creates a fast sync protocol
// manager whose snap head TD is missing and cannot be reconstructed. The fast
// head points at a block whose TD entry and parent header were deleted from
// the database, so RepairMissingTd walks into a dead end and fails while the
// canonical block head keeps its TD entry. Fast sync mode is re-enabled
// explicitly: NewProtocolManager disables it on non-empty chains, and the
// snap branch under test is only reachable with fast sync active.
func newTestProtocolManagerWithUnrepairableSnapTd(t *testing.T) (*ProtocolManager, ethdb.Database) {
	var (
		evmux  = new(event.TypeMux)
		engine = ethash.NewFaker()
		db     = rawdb.NewMemoryDatabase()
		gspec  = &core.Genesis{
			Alloc:  types.GenesisAlloc{testBank: {Balance: new(big.Int).SetUint64(10000000000000000000)}},
			Config: params.TestChainConfig,
		}
	)
	genesis := gspec.MustCommit(db)
	blockchain, err := core.NewBlockChain(db, nil, gspec, engine, vm.Config{})
	if err != nil {
		t.Fatalf("Failed to create test blockchain: %v", err)
	}
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, 512, nil)
	if _, err := blockchain.InsertChain(chain); err != nil {
		t.Fatalf("Failed to insert test chain: %v", err)
	}
	// Point the fast head at block 256 and make its TD irreparable: the TD
	// entry is gone and so is the parent's header, so the repair walk fails
	// at block 255 while the block head (512) keeps its TD entry.
	snap := chain[255]
	rawdb.WriteHeadFastBlockHash(db, snap.Hash())
	rawdb.DeleteTd(db, snap.Hash(), snap.NumberU64())
	rawdb.DeleteHeader(db, snap.ParentHash(), snap.NumberU64()-1)

	// Reload a fresh blockchain from the mutated database so the in-memory
	// caches cannot mask the deleted entries.
	blockchain, err = core.NewBlockChain(db, nil, gspec, engine, vm.Config{})
	if err != nil {
		t.Fatalf("Failed to reload test blockchain: %v", err)
	}
	pm, err := NewProtocolManager(gspec.Config, downloader.FastSync, ethconfig.Defaults.NetworkId, evmux, &testTxPool{pool: make(map[common.Hash]*types.Transaction)}, engine, blockchain, db)
	if err != nil {
		t.Fatalf("Failed to create protocol manager: %v", err)
	}
	atomic.StoreUint32(&pm.snapSync, 1)
	pm.Start(1000)
	return pm, db
}

// testTxPool is a fake, helper transaction pool for testing purposes
type testTxPool struct {
	txFeed event.Feed
	pool   map[common.Hash]*types.Transaction // Hash map of collected transactions
	added  chan<- []*types.Transaction        // Notification channel for new transactions

	lock sync.RWMutex // Protects the transaction pool
}

// Has returns an indicator whether txpool has a transaction
// cached with the given hash.
func (p *testTxPool) Has(hash common.Hash) bool {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash] != nil
}

// Get retrieves the transaction from local txpool with given
// tx hash.
func (p *testTxPool) Get(hash common.Hash) *types.Transaction {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.pool[hash]
}

// newTestTxPool creates a mock transaction pool.
func newTestTxPool() *testTxPool {
	return &testTxPool{
		pool: make(map[common.Hash]*types.Transaction),
	}
}

// Add appends a batch of transactions to the pool, and notifies any
// listeners if the addition channel is non nil
func (p *testTxPool) Add(txs []*types.Transaction, sync bool) []error {
	p.lock.Lock()
	defer p.lock.Unlock()

	for _, tx := range txs {
		p.pool[tx.Hash()] = tx
	}
	if p.added != nil {
		p.added <- txs
	}
	p.txFeed.Send(core.NewTxsEvent{Txs: txs})
	return make([]error, len(txs))
}

// Pending returns all the transactions known to the pool
func (p *testTxPool) Pending(filter txpool.PendingFilter) map[common.Address][]*txpool.LazyTransaction {
	p.lock.RLock()
	defer p.lock.RUnlock()

	batches := make(map[common.Address][]*types.Transaction)
	for _, tx := range p.pool {
		from, _ := types.Sender(types.HomesteadSigner{}, tx)
		batches[from] = append(batches[from], tx)
	}
	for _, batch := range batches {
		sort.Sort(types.TxByNonce(batch))
	}
	pending := make(map[common.Address][]*txpool.LazyTransaction)
	for addr, batch := range batches {
		for _, tx := range batch {
			pending[addr] = append(pending[addr], &txpool.LazyTransaction{
				Hash:      tx.Hash(),
				Tx:        tx,
				Time:      tx.Time(),
				GasFeeCap: uint256.MustFromBig(tx.GasFeeCap()),
				GasTipCap: uint256.MustFromBig(tx.GasTipCap()),
			})
		}
	}
	return pending
}

// SubscribeTransactions should return an event subscription of NewTxsEvent and
// send events to the given channel.
func (p *testTxPool) SubscribeTransactions(ch chan<- core.NewTxsEvent, reorgs bool) event.Subscription {
	return p.txFeed.Subscribe(ch)
}

// SubscribeNewTxsEvent should return an event subscription of NewTxsEvent and
// send events to the given channel.
func (p *testTxPool) SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription {
	return p.SubscribeTransactions(ch, false)
}

// newTestTransaction create a new dummy transaction.
func newTestTransaction(from *ecdsa.PrivateKey, nonce uint64, datasize int) *types.Transaction {
	tx := types.NewTransaction(nonce, common.Address{}, big.NewInt(0), 100000, big.NewInt(0), make([]byte, datasize))
	tx, _ = types.SignTx(tx, types.HomesteadSigner{}, from)
	return tx
}

// testPeer is a simulated peer to allow testing direct network calls.
type testPeer struct {
	net p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
	*peer
}

// newTestPeer creates a new peer registered at the given protocol manager.
func newTestPeer(name string, version int, pm *ProtocolManager, shake bool) (*testPeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Generate a random id and create the peer
	var id enode.ID
	rand.Read(id[:])

	peer := pm.newPeer(version, p2p.NewPeer(id, name, nil), net, pm.txpool.Get)

	// Start the peer on a new thread
	errc := make(chan error, 1)
	go func() {
		select {
		case pm.newPeerCh <- peer:
			errc <- pm.handle(peer)
		case <-pm.quitSync:
			errc <- p2p.DiscQuitting
		}
	}()
	tp := &testPeer{app: app, net: net, peer: peer}
	// Execute any implicitly requested handshakes and return
	if shake {
		var (
			genesis = pm.blockchain.Genesis()
			head    = pm.blockchain.CurrentHeader()
			td      = pm.blockchain.GetTd(head.Hash(), head.Number.Uint64())
		)
		tp.handshake(nil, td, head.Hash(), genesis.Hash(), forkid.NewID(pm.blockchain.Config(), genesis, head.Number.Uint64()), forkid.NewFilter(pm.blockchain))
	}
	return tp, errc
}

// handshake simulates a trivial handshake that expects the same state from the
// remote side as we are simulating locally.
func (p *testPeer) handshake(t *testing.T, td *big.Int, head common.Hash, genesis common.Hash, forkID forkid.ID, forkFilter forkid.Filter) {
	var msg interface{}
	switch p.version {
	case xdc165, xdc164:
		msg = &statusData{
			ProtocolVersion: uint32(p.version),
			NetworkID:       ethconfig.Defaults.NetworkId,
			TD:              td,
			Head:            head,
			Genesis:         genesis,
			ForkID:          forkID,
		}
	case xdc100:
		msg = &statusData100{
			ProtocolVersion: uint32(p.version),
			NetworkId:       ethconfig.Defaults.NetworkId,
			TD:              td,
			CurrentBlock:    head,
			GenesisBlock:    genesis,
		}
	default:
		panic(fmt.Sprintf("unsupported eth protocol version: %d", p.version))
	}
	if err := p2p.ExpectMsg(p.app, StatusMsg, msg); err != nil {
		t.Fatalf("status recv: %v", err)
	}
	if err := p2p.Send(p.app, StatusMsg, msg); err != nil {
		t.Fatalf("status send: %v", err)
	}
}

// close terminates the local side of the peer, notifying the remote protocol
// manager of termination.
func (p *testPeer) close() {
	p.app.Close()
}
