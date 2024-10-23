// Copyright 2014 The go-ethereum Authors
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

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"sync/atomic"

	"github.com/XinFinOrg/XDPoSChain/XDCxlending"

	"github.com/XinFinOrg/XDPoSChain/XDCx"
	"github.com/XinFinOrg/XDPoSChain/accounts"
	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/state"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/eth/downloader"
	"github.com/XinFinOrg/XDPoSChain/ethdb"
	"github.com/XinFinOrg/XDPoSChain/event"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	AccountManager() *accounts.Manager
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
	ChainDb() ethdb.Database
	GetXDCX() *XDCx.XDCX
	OrderPool() *core.OrderPool
	LendingPool() *core.LendingPool
	GetXDCXLending() *XDCxlending.Lending
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux *event.TypeMux

	worker *worker

	coinbase common.Address
	mining   int32
	eth      Backend
	engine   consensus.Engine

	canStart    int32 // can start indicates whether we can start the mining operation
	shouldStart int32 // should start indicates whether we should start after sync
}

func New(eth Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, announceTxs bool) *Miner {
	miner := &Miner{
		eth:      eth,
		mux:      mux,
		engine:   engine,
		worker:   newWorker(config, engine, common.Address{}, eth, mux, announceTxs),
		canStart: 1,
	}
	miner.Register(NewCpuAgent(eth.BlockChain(), engine))
	go miner.update()

	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
func (m *Miner) update() {
	events := m.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	for ev := range events.Chan() {
		switch ev.Data.(type) {
		case downloader.StartEvent:
			atomic.StoreInt32(&m.canStart, 0)
			if m.Mining() {
				m.Stop()
				atomic.StoreInt32(&m.shouldStart, 1)
				log.Info("Mining aborted due to sync")
			}
		case downloader.DoneEvent, downloader.FailedEvent:
			shouldStart := atomic.LoadInt32(&m.shouldStart) == 1

			atomic.StoreInt32(&m.canStart, 1)
			atomic.StoreInt32(&m.shouldStart, 0)
			if shouldStart {
				m.Start(m.coinbase)
			}
		}
	}
}

func (m *Miner) Start(coinbase common.Address) {
	atomic.StoreInt32(&m.shouldStart, 1)
	m.SetEtherbase(coinbase)

	if atomic.LoadInt32(&m.canStart) == 0 {
		log.Info("Network syncing, will start miner afterwards")
		return
	}
	atomic.StoreInt32(&m.mining, 1)

	log.Info("Starting mining operation")
	m.worker.start()
	m.worker.commitNewWork()
}

func (m *Miner) Stop() {
	m.worker.stop()
	atomic.StoreInt32(&m.mining, 0)
	atomic.StoreInt32(&m.shouldStart, 0)
}

func (m *Miner) Register(agent Agent) {
	if m.Mining() {
		agent.Start()
	}
	m.worker.register(agent)
}

func (m *Miner) Unregister(agent Agent) {
	m.worker.unregister(agent)
}

func (m *Miner) Mining() bool {
	return atomic.LoadInt32(&m.mining) > 0
}

func (m *Miner) HashRate() (tot int64) {
	if pow, ok := m.engine.(consensus.PoW); ok {
		tot += int64(pow.Hashrate())
	}
	// do we care this might race? is it worth we're rewriting some
	// aspects of the worker/locking up agents so we can get an accurate
	// hashrate?
	for agent := range m.worker.agents {
		if _, ok := agent.(*CpuAgent); !ok {
			tot += agent.GetHashRate()
		}
	}
	return
}

func (m *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	m.worker.setExtra(extra)
	return nil
}

// Pending returns the currently pending block and associated state.
func (m *Miner) Pending() (*types.Block, *state.StateDB) {
	return m.worker.pending()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
func (m *Miner) PendingBlock() *types.Block {
	return m.worker.pendingBlock()
}

// PendingBlockAndReceipts returns the currently pending block and corresponding receipts.
func (m *Miner) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return m.worker.pendingBlockAndReceipts()
}

func (m *Miner) SetEtherbase(addr common.Address) {
	m.coinbase = addr
	m.worker.setEtherbase(addr)
}

// SubscribePendingLogs starts delivering logs from pending transactions
// to the given channel.
func (m *Miner) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	return m.worker.pendingLogsFeed.Subscribe(ch)
}
