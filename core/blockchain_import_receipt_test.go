// Copyright 2026 The go-ethereum Authors
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

package core

import (
	"errors"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/consensus/ethash"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/core/vm"
	"github.com/XinFinOrg/XDPoSChain/crypto"
	"github.com/XinFinOrg/XDPoSChain/ethdb"
	"github.com/XinFinOrg/XDPoSChain/params"
)

// TestInsertReceiptChainReportsInterruption covers the interruption check of a receipt
// batch. The blocks before the one that stopped the batch may already have been flushed
// to disk, so reporting a nil error would claim the whole batch had been written - the
// very thing an interrupted insertion must not claim. The sentinel is local, so the
// downloader reports it as errLocalInsertFailure: it neither drops the peer that served
// the batch nor presents the failure as a cancellation somebody requested.
func TestInsertReceiptChainReportsInterruption(t *testing.T) {
	chain, blocks := newInsertChainTester(t, nil, 5, 5)
	receipts := make([]types.Receipts, len(blocks))

	chain.InterruptInsert(true)
	defer chain.InterruptInsert(false)

	n, err := chain.InsertReceiptChain(blocks, receipts)
	if !errors.Is(err, ErrInsertionInterrupted) {
		t.Fatalf("unexpected error: have %v want %v", err, ErrInsertionInterrupted)
	}
	if want := 0; n != want {
		t.Fatalf("unexpected failing index: have %d want %d", n, want)
	}
}

// errRefusedWrite is what the batch below reports, standing for a local write the database
// refused (a full disk, a read-only mount, an I/O error).
var errRefusedWrite = errors.New("the database refused the write")

// refusingBatchDB hands out batches that can be told to fail their write, so that the failure
// path of a receipt import can be driven without a real disk failing.
type refusingBatchDB struct {
	ethdb.Database
	refuse atomic.Bool
}

func (db *refusingBatchDB) NewBatch() ethdb.Batch {
	return &refusingBatch{Batch: db.Database.NewBatch(), refuse: &db.refuse}
}

type refusingBatch struct {
	ethdb.Batch
	refuse *atomic.Bool
}

func (b *refusingBatch) Write() error {
	if b.refuse.Load() {
		return errRefusedWrite
	}
	return b.Batch.Write()
}

// TestInsertReceiptChainReportsARefusedWrite covers a write the database refuses while a receipt
// batch is being flushed. It says nothing about the peer that served the receipts, so it is
// reported as the local condition it is - the one the downloader answers by cancelling the
// content processing instead of dropping the peer that supplied valid data.
func TestInsertReceiptChainReportsARefusedWrite(t *testing.T) {
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		gspec   = &Genesis{
			Alloc:   types.GenesisAlloc{address: {Balance: big.NewInt(1000000000000000)}},
			BaseFee: big.NewInt(params.InitialBaseFee),
			Config:  params.TestChainConfig,
		}
	)
	db := &refusingBatchDB{Database: rawdb.NewMemoryDatabase()}
	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 5, nil)
	chain, err := NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Headers only: a receipt import completes a header chain, and a block that is already there
	// would be skipped instead of written.
	headers := make([]*types.Header, len(blocks))
	for i, block := range blocks {
		headers[i] = block.Header()
	}
	if n, err := chain.InsertHeaderChain(headers, 1); err != nil {
		t.Fatalf("header %d: failed to insert headers: %v", n, err)
	}
	receipts := make([]types.Receipts, len(blocks))

	db.refuse.Store(true)
	defer db.refuse.Store(false)

	n, err := chain.InsertReceiptChain(blocks, receipts)
	if !errors.Is(err, errRefusedWrite) {
		t.Fatalf("index %d: the refused write must be reported with its cause, have %v", n, err)
	}
	if !errors.Is(err, ErrLocalInsertCondition) {
		t.Fatalf("index %d: a refused write must be reported as a local condition, have %v", n, err)
	}
	if !IsLocalInsertError(err) {
		t.Fatalf("a refused write must not be blamed on the peer: %v", err)
	}
}
