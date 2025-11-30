package scaner

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus/ethash"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/core/vm"
	"github.com/XinFinOrg/XDPoSChain/params"
)

func TestOwnerFeeRoutingActive(t *testing.T) {
	chainConfig := &params.ChainConfig{
		TIPTRC21FeeBlock: big.NewInt(900),
	}
	threshold := new(big.Int).Set(chainConfig.TIPTRC21FeeBlock)
	if ownerFeeRoutingActive(threshold, chainConfig) {
		t.Fatal("owner fee routing should be inactive at TIPTRC21Fee boundary")
	}
	if !ownerFeeRoutingActive(new(big.Int).Add(threshold, big.NewInt(1)), chainConfig) {
		t.Fatal("owner fee routing should be active after TIPTRC21Fee")
	}
}

func TestKycTask_SkipsBlocksBeforeOwnerFeeRoutingActivation(t *testing.T) {
	bc := newScanTaskTestChain(t, 4)
	out := t.TempDir()
	stateDir := t.TempDir()

	chainConfig := &params.ChainConfig{
		TIPTRC21FeeBlock: big.NewInt(900),
	}
	originalThreshold := new(big.Int).Set(chainConfig.TIPTRC21FeeBlock)
	chainConfig.TIPTRC21FeeBlock = big.NewInt(2)
	defer func() {
		chainConfig.TIPTRC21FeeBlock = originalThreshold
	}()

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      stateDir,
		Kyc: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)

	if err := task.processUntil(bc, 2, nil); err != nil {
		t.Fatalf("processUntil error: %v", err)
	}
	if task.state.BlockNumber != 2 {
		t.Fatalf("block number = %d, want 2", task.state.BlockNumber)
	}

	output, err := os.ReadFile(task.filePath)
	if err != nil {
		t.Fatalf("read kyc output file: %v", err)
	}
	wantOutput := "block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx\n"
	if string(output) != wantOutput {
		t.Fatalf("kyc output = %q, want header only %q", string(output), wantOutput)
	}

	if err := task.flushState(); err != nil {
		t.Fatalf("flushState() error: %v", err)
	}
	blob, err := os.ReadFile(task.statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var saved scanTaskState
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	if saved.BlockNumber != 2 {
		t.Fatalf("saved BlockNumber = %d, want 2", saved.BlockNumber)
	}
}

func TestNewKycTask_TruncatesStaleTailBeyondSavedState(t *testing.T) {
	bc := newScanTaskTestChain(t, 12)
	outputDir := t.TempDir()
	stateDir := t.TempDir()
	filePath := filepath.Join(outputDir, "kyc_result.txt")
	content := strings.Join([]string{
		"block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx",
		"7 0xaaa 0 0x111 voteInvalidKYC true true true false",
		"8 0xbbb 0 0x222 voteInvalidKYC true true false false",
		"# warning block=9 txs=1 receipts=0",
		"10 0xccc 1 0x333 voteInvalidKYC true false false true",
	}, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write kyc result file: %v", err)
	}

	block := bc.GetBlockByNumber(8)
	if block == nil {
		t.Fatal("missing block 8")
	}
	statePath := filepath.Join(stateDir, "kyc_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 8, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 2,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		Kyc:           ScanTaskConfig{Enabled: true, FromBlock: 3},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.next != 9 {
		t.Fatalf("task.next = %d, want 9", task.next)
	}
	trimmed, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read kyc result file: %v", err)
	}
	want := strings.Join([]string{
		"block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx",
		"7 0xaaa 0 0x111 voteInvalidKYC true true true false",
		"8 0xbbb 0 0x222 voteInvalidKYC true true false false",
	}, "\n") + "\n"
	if string(trimmed) != want {
		t.Fatalf("trimmed kyc output = %q, want %q", string(trimmed), want)
	}
}

func TestNewKycTask_RewindsWhenOutputFileLagsBehindState(t *testing.T) {
	bc := newScanTaskTestChain(t, 12)
	outputDir := t.TempDir()
	stateDir := t.TempDir()
	filePath := filepath.Join(outputDir, "kyc_result.txt")
	content := strings.Join([]string{
		"block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx",
		"7 0xaaa 0 0x111 voteInvalidKYC true true true false",
	}, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write kyc result file: %v", err)
	}

	block := bc.GetBlockByNumber(8)
	if block == nil {
		t.Fatal("missing block 8")
	}
	statePath := filepath.Join(stateDir, "kyc_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 8, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 2,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		Kyc:           ScanTaskConfig{Enabled: true, FromBlock: 3},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.next != 9 {
		t.Fatalf("task.next = %d, want 9 (continue from state, not rewind)", task.next)
	}
}

func TestNewKycTask_UsesFixedResultFilePath(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 2)
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		Kyc:           ScanTaskConfig{Enabled: true, FromBlock: 0},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.filePath != filepath.Join(outputDir, "kyc_result.txt") {
		t.Fatalf("filePath = %q, want %q", task.filePath, filepath.Join(outputDir, "kyc_result.txt"))
	}
	if task.statePath != filepath.Join(stateDir, "kyc_state.json") {
		t.Fatalf("statePath = %q, want %q", task.statePath, filepath.Join(stateDir, "kyc_state.json"))
	}
	content, err := os.ReadFile(task.filePath)
	if err != nil {
		t.Fatalf("read kyc file: %v", err)
	}
	wantHeader := "block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx\n"
	if string(content) != wantHeader {
		t.Fatalf("kyc file content = %q, want %q", string(content), wantHeader)
	}
}

func TestKycTask_ProcessUntilWithoutParentStateAccess(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{
		Config:   params.TestChainConfig,
		GasLimit: 100_000_000,
		Alloc: types.GenesisAlloc{
			testBank: {Balance: new(big.Int).SetUint64(10_000_000_000_000_000)},
		},
	}
	genesis := gspec.MustCommit(db)
	bc, err := core.NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer bc.Stop()

	selector := common.FromHex("0xf2ee3c7d")
	input := append(append([]byte{}, selector...), common.LeftPadBytes(common.HexToAddress("0x00000000000000000000000000000000000000aa").Bytes(), 32)...)
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, 2, func(i int, block *core.BlockGen) {
		if i != 1 {
			return
		}
		signer := types.MakeSigner(gspec.Config, block.Number())
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   gspec.Config.ChainID,
			Nonce:     block.TxNonce(testBank),
			GasTipCap: big.NewInt(1_000_000_000),
			GasFeeCap: big.NewInt(20_000_000_000),
			Gas:       150000,
			To:        &common.MasternodeVotingSMCBinary,
			Value:     big.NewInt(0),
			Data:      input,
		}), signer, testBankKey)
		if err != nil {
			t.Fatalf("failed to sign KYC tx: %v", err)
		}
		block.AddTx(tx)
	})
	if _, err := bc.InsertChain(chain); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	parent := bc.GetBlockByNumber(1)
	if parent == nil {
		t.Fatal("missing parent block")
	}
	_ = bc.TrieDB().Dereference(parent.Root())
	rawdb.DeleteLegacyTrieNode(db, parent.Root())

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     t.TempDir(),
		StateDir:      t.TempDir(),
		Kyc:           ScanTaskConfig{Enabled: true, FromBlock: 1},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	defer task.close()

	if err := task.processUntil(bc, 2, nil); err != nil {
		t.Fatalf("processUntil error: %v", err)
	}
	output, err := os.ReadFile(task.filePath)
	if err != nil {
		t.Fatalf("read kyc output file: %v", err)
	}
	if !strings.Contains(string(output), "voteInvalidKYC") {
		t.Fatalf("kyc output = %q, want voteInvalidKYC entry", string(output))
	}
}

func TestKycTask_ProcessUntilFailsOnMissingReceipts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{
		Config:   params.TestChainConfig,
		GasLimit: 100_000_000,
		Alloc: types.GenesisAlloc{
			testBank: {Balance: new(big.Int).SetUint64(10_000_000_000_000_000)},
		},
	}
	genesis := gspec.MustCommit(db)
	bc, err := core.NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer bc.Stop()

	selector := common.FromHex("0xf2ee3c7d")
	input := append(append([]byte{}, selector...), common.LeftPadBytes(common.HexToAddress("0x00000000000000000000000000000000000000aa").Bytes(), 32)...)
	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, 2, func(i int, block *core.BlockGen) {
		if i != 1 {
			return
		}
		signer := types.MakeSigner(gspec.Config, block.Number())
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   gspec.Config.ChainID,
			Nonce:     block.TxNonce(testBank),
			GasTipCap: big.NewInt(1_000_000_000),
			GasFeeCap: big.NewInt(20_000_000_000),
			Gas:       150000,
			To:        &common.MasternodeVotingSMCBinary,
			Value:     big.NewInt(0),
			Data:      input,
		}), signer, testBankKey)
		if err != nil {
			t.Fatalf("failed to sign KYC tx: %v", err)
		}
		block.AddTx(tx)
	})
	if _, err := bc.InsertChain(chain); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}
	block := bc.GetBlockByNumber(2)
	if block == nil {
		t.Fatal("missing block 2")
	}
	rawdb.DeleteReceipts(db, block.Hash(), block.NumberU64())

	task, err := newKycTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     t.TempDir(),
		StateDir:      t.TempDir(),
		Kyc:           ScanTaskConfig{Enabled: true, FromBlock: 1},
	})
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	defer task.close()

	err = task.processUntil(bc, 2, nil)
	if err == nil {
		t.Fatal("expected processUntil to fail when receipts are missing")
	}
	if !strings.Contains(err.Error(), "receipts") {
		t.Fatalf("error = %v, want receipts-related failure", err)
	}
}

func TestCollectVoteInvalidKYCTransactions(t *testing.T) {
	selector := common.FromHex("0xf2ee3c7d")
	kycData := append(append([]byte{}, selector...), make([]byte, 32)...)

	txs := types.Transactions{
		types.NewTransaction(0, common.HexToAddress("0x0000000000000000000000000000000000000001"), big.NewInt(0), 21000, big.NewInt(1), nil),
		types.NewTransaction(1, common.MasternodeVotingSMCBinary, big.NewInt(0), 50000, big.NewInt(1), kycData),
		types.NewTransaction(2, common.HexToAddress("0x0000000000000000000000000000000000000002"), big.NewInt(0), 21000, big.NewInt(1), nil),
		types.NewTransaction(3, common.MasternodeVotingSMCBinary, big.NewInt(0), 50000, big.NewInt(1), []byte{0x01, 0x02, 0x03, 0x04}),
		types.NewTransaction(4, common.MasternodeVotingSMCBinary, big.NewInt(0), 50000, big.NewInt(1), kycData),
	}

	indices, hasLaterNormal := collectVoteInvalidKYCTransactions(txs, selector)
	if !reflect.DeepEqual(indices, []int{1, 4}) {
		t.Fatalf("indices = %v, want [1 4]", indices)
	}
	if !reflect.DeepEqual(hasLaterNormal, []bool{true, false}) {
		t.Fatalf("hasLaterNormal = %v, want [true false]", hasLaterNormal)
	}
}

func TestInvalidatedNodeIncludesCandidate(t *testing.T) {
	candidate := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	other := common.HexToAddress("0x00000000000000000000000000000000000000bb")

	encodeInvalidatedData := func(candidates []common.Address) []byte {
		// ABI layout for InvalidatedNode(address,address[]):
		// 0x00: owner (32 bytes)
		// 0x20: offset to address[] data (32 bytes, value 0x40)
		// 0x40: array length (32 bytes)
		// 0x60: elements (each 32 bytes, right-aligned address)
		out := make([]byte, 32+32+32+32*len(candidates))
		out[63] = 0x40
		out[95] = byte(len(candidates))
		for i, addr := range candidates {
			start := 96 + i*32
			copy(out[start+12:start+32], addr.Bytes())
		}
		return out
	}

	withCandidate := encodeInvalidatedData([]common.Address{other, candidate})
	if !invalidatedNodeIncludesCandidate(withCandidate, candidate) {
		t.Fatal("expected candidate to be found in invalidated node payload")
	}

	withoutCandidate := encodeInvalidatedData([]common.Address{other})
	if invalidatedNodeIncludesCandidate(withoutCandidate, candidate) {
		t.Fatal("did not expect candidate in invalidated node payload")
	}

	if invalidatedNodeIncludesCandidate([]byte{1, 2, 3}, candidate) {
		t.Fatal("did not expect candidate in malformed payload")
	}
}

func TestInvalidatedNodeIncludesCandidateRejectsOverflowLength(t *testing.T) {
	candidate := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	payload := make([]byte, 96)
	payload[63] = 0x40
	new(big.Int).SetUint64(math.MaxUint64/32 + 1).FillBytes(payload[64:96])

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("invalidatedNodeIncludesCandidate panicked on malformed payload: %v", r)
		}
	}()
	if invalidatedNodeIncludesCandidate(payload, candidate) {
		t.Fatal("did not expect candidate match for overflow-length payload")
	}
}
