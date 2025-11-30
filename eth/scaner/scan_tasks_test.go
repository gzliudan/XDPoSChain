package scaner

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/consensus/ethash"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/rawdb"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/core/vm"
	"github.com/XinFinOrg/XDPoSChain/params"
)

func newScanTaskTestChain(t *testing.T, blocks int) *core.BlockChain {
	t.Helper()

	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	genesis := gspec.MustCommit(db)

	bc, err := core.NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}

	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, blocks, nil)
	if _, err := bc.InsertChain(chain); err != nil {
		bc.Stop()
		t.Fatalf("failed to insert chain: %v", err)
	}
	t.Cleanup(func() { bc.Stop() })
	return bc
}

func newScanTaskTxCountChain(t *testing.T, txCounts []int) *core.BlockChain {
	t.Helper()

	db := rawdb.NewMemoryDatabase()
	fundedBalance := new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)
	gspec := &core.Genesis{
		Config:   params.TestChainConfig,
		GasLimit: 100_000_000,
		Alloc: types.GenesisAlloc{
			testBank: {Balance: fundedBalance},
		},
	}
	genesis := gspec.MustCommit(db)

	bc, err := core.NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}

	chain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, len(txCounts), func(i int, block *core.BlockGen) {
		signer := types.MakeSigner(gspec.Config, block.Number())
		for j := 0; j < txCounts[i]; j++ {
			to := common.BytesToAddress([]byte{byte(i + 1), byte((j % 250) + 1)})
			tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
				ChainID:   gspec.Config.ChainID,
				Nonce:     block.TxNonce(testBank),
				GasTipCap: big.NewInt(1_000_000_000),
				GasFeeCap: big.NewInt(20_000_000_000),
				Gas:       params.TxGas,
				To:        &to,
				Value:     big.NewInt(1),
				Data:      nil,
			}), signer, testBankKey)
			if err != nil {
				t.Fatalf("failed to sign tx for block %d tx %d: %v", i, j, err)
			}
			block.AddTx(tx)
		}
	})
	if _, err := bc.InsertChain(chain); err != nil {
		bc.Stop()
		t.Fatalf("failed to insert tx chain: %v", err)
	}
	t.Cleanup(func() { bc.Stop() })
	return bc
}

func TestResolveNextBlock_RewindOnStateHashMismatch(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 12)
	state := scanTaskState{
		BlockNumber: 10,
		BlockHash:   "0xdeadbeef",
	}
	plan := resolveNextBlock(bc, 3, state, true, "txinfo")
	if plan.Next != 3 {
		t.Fatalf("Next = %d, want 3 for a conservative rewind to the configured start block", plan.Next)
	}
	if !plan.ResetOutput {
		t.Fatal("expected ResetOutput to be true on state hash mismatch")
	}
	if !plan.HasState {
		t.Fatal("expected HasState to be true after rewind")
	}
	if plan.State.BlockNumber != 2 {
		t.Fatalf("rewound BlockNumber = %d, want 2", plan.State.BlockNumber)
	}
	safeBlock := bc.GetBlockByNumber(2)
	if safeBlock == nil {
		t.Fatal("missing safe block 2")
	}
	if plan.State.BlockHash != safeBlock.Hash().Hex() {
		t.Fatalf("rewound BlockHash = %q, want %q", plan.State.BlockHash, safeBlock.Hash().Hex())
	}
}

func TestResolveNextBlock_ResumeOnMatchingState(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 12)
	block := bc.GetBlockByNumber(10)
	if block == nil {
		t.Fatal("missing block 10")
	}
	state := scanTaskState{
		BlockNumber: 10,
		BlockHash:   block.Hash().Hex(),
	}
	plan := resolveNextBlock(bc, 3, state, true, "kyc")
	if plan.Next != 11 {
		t.Fatalf("Next = %d, want 11", plan.Next)
	}
	if plan.ResetOutput {
		t.Fatal("did not expect ResetOutput for matching state")
	}
	if !plan.HasState || plan.State.BlockNumber != 10 || plan.State.BlockHash != block.Hash().Hex() {
		t.Fatalf("State = %+v, want block 10 with matching hash", plan.State)
	}
}

func TestResolveNextBlock_RewindsWhenStartBlockMovesBackward(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 12)
	block := bc.GetBlockByNumber(10)
	if block == nil {
		t.Fatal("missing block 10")
	}
	state := scanTaskState{
		BlockNumber: 10,
		BlockHash:   block.Hash().Hex(),
		StartBlock:  8,
	}
	plan := resolveNextBlock(bc, 3, state, true, "txinfo")
	if plan.Next != 3 {
		t.Fatalf("Next = %d, want 3 when the configured start block moves backward", plan.Next)
	}
	if !plan.ResetOutput {
		t.Fatal("expected ResetOutput to be true when the configured start block moves backward")
	}
	if !plan.HasState {
		t.Fatal("expected HasState to be true after rewind")
	}
	if plan.State.BlockNumber != 2 {
		t.Fatalf("rewound BlockNumber = %d, want 2", plan.State.BlockNumber)
	}
}

func TestResolveNextBlock_ClampsRollbackToCurrentHead(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 12)
	if err := bc.SetHead(5); err != nil {
		t.Fatalf("SetHead error: %v", err)
	}
	state := scanTaskState{
		BlockNumber: 10,
		BlockHash:   "0xdeadbeef",
	}
	plan := resolveNextBlock(bc, 1, state, true, "txinfo")
	if plan.Next != 1 {
		t.Fatalf("Next = %d, want 1 when rewinding conservatively after the saved state becomes untrusted", plan.Next)
	}
	if !plan.HasState || plan.State.BlockNumber != 0 {
		t.Fatalf("State = %+v, want block 0", plan.State)
	}
}

func TestNewTxInfoTask_DoesNotRewriteAlignedOutputOnResume(t *testing.T) {
	bc := newScanTaskTxCountChain(t, []int{1, 1, 1})
	outputDir := t.TempDir()
	stateDir := t.TempDir()
	filePath := filepath.Join(outputDir, "txinfo_result.txt")
	content := strings.Join([]string{
		"0x111 xdc0000000000000000000000000000000000000001 1",
		"0x222 xdc0000000000000000000000000000000000000002 2",
	}, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write txinfo result file: %v", err)
	}
	before, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat txinfo result file: %v", err)
	}
	block := bc.GetBlockByNumber(2)
	if block == nil {
		t.Fatal("missing block 2")
	}
	statePath := filepath.Join(stateDir, "txinfo_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 2, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 1},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	defer task.close()

	after, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat txinfo result file after resume: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("aligned txinfo output should not be rewritten on resume")
	}
}

func TestNewTxInfoTask_RewindsWhenOutputFileIsMissing(t *testing.T) {
	bc := newScanTaskTxCountChain(t, []int{1, 1, 1})
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	block := bc.GetBlockByNumber(2)
	if block == nil {
		t.Fatal("missing block 2")
	}
	statePath := filepath.Join(stateDir, "txinfo_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 2, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 1},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	defer task.close()

	if task.next != 1 {
		t.Fatalf("task.next = %d, want 1 after rewinding to rebuild a missing result file", task.next)
	}
	if task.state.BlockNumber != 0 {
		t.Fatalf("task.state.BlockNumber = %d, want 0 after rewind", task.state.BlockNumber)
	}
}

func TestNewTxInfoTask_RewindsWhenOutputFileLagsBehindState(t *testing.T) {
	bc := newScanTaskTxCountChain(t, []int{1, 1, 1})
	outputDir := t.TempDir()
	stateDir := t.TempDir()
	filePath := filepath.Join(outputDir, "txinfo_result.txt")
	content := "0x111 xdc0000000000000000000000000000000000000001 1\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write txinfo result file: %v", err)
	}

	block := bc.GetBlockByNumber(2)
	if block == nil {
		t.Fatal("missing block 2")
	}
	statePath := filepath.Join(stateDir, "txinfo_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 2, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 1},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	defer task.close()

	if task.next != 1 {
		t.Fatalf("task.next = %d, want 1 after rewinding to fill missing historical output", task.next)
	}
}

func TestResolveNextBlock_RewindsToCommonAncestorOnDeepReorg(t *testing.T) {
	t.Parallel()

	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
	genesis := gspec.MustCommit(db)

	bc, err := core.NewBlockChain(db, nil, gspec, ethash.NewFaker(), vm.Config{})
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer bc.Stop()

	baseChain, _ := core.GenerateChain(gspec.Config, genesis, ethash.NewFaker(), db, 12, nil)
	if _, err := bc.InsertChain(baseChain); err != nil {
		t.Fatalf("failed to insert base chain: %v", err)
	}
	oldBlock10 := bc.GetBlockByNumber(10)
	if oldBlock10 == nil {
		t.Fatal("missing original block 10")
	}
	forkBase := bc.GetBlockByNumber(4)
	if forkBase == nil {
		t.Fatal("missing fork base block 4")
	}
	if err := bc.SetHead(4); err != nil {
		t.Fatalf("SetHead error: %v", err)
	}
	altChain, _ := core.GenerateChain(gspec.Config, forkBase, ethash.NewFaker(), db, 8, func(i int, block *core.BlockGen) {
		block.SetExtra([]byte{byte(i + 1), 0xaa})
	})
	if _, err := bc.InsertChain(altChain); err != nil {
		t.Fatalf("failed to insert fork chain: %v", err)
	}
	newBlock10 := bc.GetBlockByNumber(10)
	if newBlock10 == nil {
		t.Fatal("missing forked block 10")
	}
	if newBlock10.Hash() == oldBlock10.Hash() {
		t.Fatal("expected canonical block 10 to change after deep reorg")
	}

	state := scanTaskState{BlockNumber: 10, BlockHash: oldBlock10.Hash().Hex()}
	plan := resolveNextBlock(bc, 1, state, true, "txinfo")
	if plan.Next > 5 {
		t.Fatalf("Next = %d, want a conservative rewind no later than block 5 after the deep reorg", plan.Next)
	}
	if !plan.ResetOutput {
		t.Fatal("expected ResetOutput to be true after deep reorg")
	}
	if !plan.HasState {
		t.Fatalf("State = %+v, want persisted canonical state after rewind", plan.State)
	}
	if plan.State.BlockNumber+1 != plan.Next {
		t.Fatalf("state/next mismatch: state=%+v next=%d", plan.State, plan.Next)
	}
}

func TestScanTaskRunner_ProcessHeadRewindsTxInfoAfterSetHead(t *testing.T) {
	bc := newScanTaskTxCountChain(t, []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1})
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 1, BatchSize: 12},
	})
	task, err := newTxInfoTask(bc, runner.cfg)
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	defer task.close()
	runner.txTask = task
	runner.txStats = newScanTaskCounters("txinfo", task.next)

	runner.processHead(12)
	if runner.txTask.next != 13 {
		t.Fatalf("txTask.next = %d, want 13 after initial scan", runner.txTask.next)
	}

	if err := bc.SetHead(5); err != nil {
		t.Fatalf("SetHead error: %v", err)
	}
	runner.processHead(5)

	if runner.txTask.next != 6 {
		t.Fatalf("txTask.next = %d, want 6 after runtime rewind", runner.txTask.next)
	}
	if runner.txTask.state.BlockNumber != 5 {
		t.Fatalf("txTask.state.BlockNumber = %d, want 5", runner.txTask.state.BlockNumber)
	}
	content, err := os.ReadFile(task.filePath)
	if err != nil {
		t.Fatalf("read txinfo result file: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		blockNumber, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("parse block number from %q: %v", line, err)
		}
		if blockNumber >= 6 {
			t.Fatalf("found stale txinfo line after rewind: %q", line)
		}
	}
}

func TestNewTxInfoTask_TruncatesOutputOnStateMismatch(t *testing.T) {
	bc := newScanTaskTestChain(t, 12)
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	filePath := filepath.Join(outputDir, "txinfo_result.txt")
	content := strings.Join([]string{
		"0xaaa 0x0000000000000000000000000000000000000001 2",
		"0xbbb 0x0000000000000000000000000000000000000002 7",
		"0xccc 0x0000000000000000000000000000000000000003 8",
		"0xddd 0x0000000000000000000000000000000000000004 10",
	}, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write txinfo result file: %v", err)
	}

	statePath := filepath.Join(stateDir, "txinfo_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 10, BlockHash: "0xdeadbeef"}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 2,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 3},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.next != 3 {
		t.Fatalf("task.next = %d, want 3 after rewinding to the configured start block", task.next)
	}
	trimmed, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read truncated txinfo file: %v", err)
	}
	want := strings.Join([]string{
		"0xaaa 0x0000000000000000000000000000000000000001 2",
	}, "\n") + "\n"
	if string(trimmed) != want {
		t.Fatalf("trimmed txinfo output = %q, want %q", string(trimmed), want)
	}

	blob, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var saved scanTaskState
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	safeBlock := bc.GetBlockByNumber(2)
	if safeBlock == nil {
		t.Fatal("missing safe block 2")
	}
	if saved.BlockNumber != 2 || saved.BlockHash != safeBlock.Hash().Hex() {
		t.Fatalf("saved state = %+v, want block 2 hash %q", saved, safeBlock.Hash().Hex())
	}
}

func TestNewTxInfoTask_TruncatesStaleTailBeyondSavedState(t *testing.T) {
	bc := newScanTaskTestChain(t, 12)
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	filePath := filepath.Join(outputDir, "txinfo_result.txt")
	content := strings.Join([]string{
		"0xaaa 0x0000000000000000000000000000000000000001 7",
		"0xbbb 0x0000000000000000000000000000000000000002 8",
		"0xccc 0x0000000000000000000000000000000000000003 9",
		"0xddd 0x0000000000000000000000000000000000000004 10",
	}, "\n") + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("write txinfo result file: %v", err)
	}

	block := bc.GetBlockByNumber(8)
	if block == nil {
		t.Fatal("missing block 8")
	}
	statePath := filepath.Join(stateDir, "txinfo_state.json")
	if err := saveScanTaskState(statePath, scanTaskState{BlockNumber: 8, BlockHash: block.Hash().Hex()}); err != nil {
		t.Fatalf("save state file: %v", err)
	}

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 2,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 3},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.next != 9 {
		t.Fatalf("task.next = %d, want 9", task.next)
	}
	trimmed, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read txinfo file: %v", err)
	}
	want := strings.Join([]string{
		"0xaaa 0x0000000000000000000000000000000000000001 7",
		"0xbbb 0x0000000000000000000000000000000000000002 8",
	}, "\n") + "\n"
	if string(trimmed) != want {
		t.Fatalf("trimmed txinfo output = %q, want %q", string(trimmed), want)
	}
}

func TestTxInfoTask_ProcessUntilDoesNotPersistStateWhenSyncFails(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTxCountChain(t, []int{1, 1})
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer file.Close()
	if err := file.Sync(); err == nil {
		t.Skipf("%s sync unexpectedly succeeded", os.DevNull)
	}

	statePath := filepath.Join(t.TempDir(), "txinfo_state.json")
	task := &txInfoTask{next: 1, startBlock: 1, statePath: statePath, file: file, filePath: os.DevNull}

	err = task.processUntil(bc, 1, nil)
	if err == nil {
		t.Fatal("processUntil should fail when output sync fails")
	}
	if task.state.BlockNumber != 0 || task.state.BlockHash != "" {
		t.Fatalf("state advanced on sync error: %+v", task.state)
	}
	if _, hasState, err := loadScanTaskState(statePath); err != nil {
		t.Fatalf("loadScanTaskState error: %v", err)
	} else if hasState {
		t.Fatal("state file should not be persisted when output sync fails")
	}
}

func TestKycTask_ProcessUntilDoesNotPersistStateWhenSyncFails(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 2)
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer file.Close()
	if err := file.Sync(); err == nil {
		t.Skipf("%s sync unexpectedly succeeded", os.DevNull)
	}

	statePath := filepath.Join(t.TempDir(), "kyc_state.json")
	task := &kycTask{next: 1, startBlock: 1, statePath: statePath, file: file, filePath: os.DevNull}

	err = task.processUntil(bc, 1, nil)
	if err == nil {
		t.Fatal("processUntil should fail when output sync fails")
	}
	if task.state.BlockNumber != 0 || task.state.BlockHash != "" {
		t.Fatalf("state advanced on sync error: %+v", task.state)
	}
	if _, hasState, err := loadScanTaskState(statePath); err != nil {
		t.Fatalf("loadScanTaskState error: %v", err)
	} else if hasState {
		t.Fatal("state file should not be persisted when output sync fails")
	}
}

func TestScanTaskRunner_StartStopTxInfo(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 6)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
			BatchSize: 1,
		},
	})

	if err := runner.start(); err != nil {
		t.Fatalf("runner.start error: %v", err)
	}
	waitForScanProgress(t, 2*time.Second, func() bool {
		return runner.txTask != nil && runner.txTask.state.BlockNumber >= 6
	})
	runner.stop()

	if runner.txTask == nil {
		t.Fatal("expected tx task to be initialized")
	}
	if runner.txTask.state.BlockNumber < 6 {
		t.Fatalf("block number = %d, want >= 6", runner.txTask.state.BlockNumber)
	}
}

func TestScanTaskRunner_StartStopKyc(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 6)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		Kyc: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	if err := runner.start(); err != nil {
		t.Fatalf("runner.start error: %v", err)
	}
	waitForScanProgress(t, 2*time.Second, func() bool {
		return runner.kycTask != nil && runner.kycTask.state.BlockNumber >= 6
	})
	runner.stop()

	if runner.kycTask == nil {
		t.Fatal("expected kyc task to be initialized")
	}
	if runner.kycTask.state.BlockNumber < 6 {
		t.Fatalf("block number = %d, want >= 6", runner.kycTask.state.BlockNumber)
	}
}

func TestScanTaskRunner_StartCreatesOutputAndStateDirs(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 1)
	root := t.TempDir()
	outputDir := filepath.Join(root, "scan-tasks")
	stateDir := filepath.Join(outputDir, "state")

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	if err := runner.start(); err != nil {
		t.Fatalf("runner.start error: %v", err)
	}
	runner.stop()

	if _, err := os.Stat(outputDir); err != nil {
		t.Fatalf("output dir should exist after start: %v", err)
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("state dir should exist after start: %v", err)
	}
}

func TestScanTaskRunner_StartErrorIsSticky(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 1)
	out := t.TempDir()
	state := t.TempDir()
	badStatePath := filepath.Join(state, "txinfo_state.json")
	if err := os.MkdirAll(badStatePath, 0o755); err != nil {
		t.Fatalf("mkdir bad txinfo state path: %v", err)
	}

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	if err := runner.start(); err == nil {
		t.Fatal("first runner.start should fail")
	}
	if err := runner.start(); err == nil {
		t.Fatal("second runner.start should return the previous initialization error")
	}
}

func TestScanTaskRunner_StartFailsOnCorruptedTxInfoState(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 1)
	out := t.TempDir()
	state := t.TempDir()
	statePath := filepath.Join(state, "txinfo_state.json")
	if err := os.WriteFile(statePath, []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("write corrupted txinfo state file: %v", err)
	}

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	err := runner.start()
	if err == nil {
		t.Fatal("runner.start should fail on corrupted txinfo state")
	}
	if !strings.Contains(err.Error(), "corrupted scan task state file") {
		t.Fatalf("start error = %v, want corrupted-state message", err)
	}
	errBlob, readErr := os.ReadFile(filepath.Join(state, "txinfo_error.json"))
	if readErr != nil {
		t.Fatalf("read txinfo error file: %v", readErr)
	}
	var record scanTaskErrorState
	if err := json.Unmarshal(errBlob, &record); err != nil {
		t.Fatalf("decode txinfo error file: %v", err)
	}
	if record.Task != "txinfo" {
		t.Fatalf("record task = %q, want txinfo", record.Task)
	}
}

func TestScanTaskRunner_StartFailsOnCorruptedKycState(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 1)
	out := t.TempDir()
	state := t.TempDir()
	statePath := filepath.Join(state, "kyc_state.json")
	if err := os.WriteFile(statePath, []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("write corrupted kyc state file: %v", err)
	}

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		Kyc: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	err := runner.start()
	if err == nil {
		t.Fatal("runner.start should fail on corrupted kyc state")
	}
	if !strings.Contains(err.Error(), "corrupted scan task state file") {
		t.Fatalf("start error = %v, want corrupted-state message", err)
	}
	errBlob, readErr := os.ReadFile(filepath.Join(state, "kyc_error.json"))
	if readErr != nil {
		t.Fatalf("read kyc error file: %v", readErr)
	}
	var record scanTaskErrorState
	if err := json.Unmarshal(errBlob, &record); err != nil {
		t.Fatalf("decode kyc error file: %v", err)
	}
	if record.Task != "kyc" {
		t.Fatalf("record task = %q, want kyc", record.Task)
	}
}

func TestScanTaskRunner_ProcessHeadRespectsConfiguredBatchSize(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 20)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		Kyc: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
			BatchSize: 3,
		},
	})
	task, err := newKycTask(bc, runner.cfg)
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)
	runner.kycTask = task

	_, kycProcessed := runner.processHead(20)
	if kycProcessed != 3 {
		t.Fatalf("kycProcessed = %d, want 3", kycProcessed)
	}
	if runner.kycTask.next != 3 {
		t.Fatalf("next = %d, want 3", runner.kycTask.next)
	}
}

func TestScanTaskRunner_ProcessHeadFlushesStateAtBatchEnd(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 20)
	out := t.TempDir()
	stateDir := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      stateDir,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
			BatchSize: 3,
		},
	})
	task, err := newTxInfoTask(bc, runner.cfg)
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)
	runner.txTask = task
	runner.txStats = newScanTaskCounters("txinfo", task.next)

	txProcessed, _ := runner.processHead(20)
	if txProcessed != 3 {
		t.Fatalf("txProcessed = %d, want 3", txProcessed)
	}
	blob, err := os.ReadFile(filepath.Join(stateDir, "txinfo_state.json"))
	if err != nil {
		t.Fatalf("read state file after batch: %v", err)
	}
	var saved scanTaskState
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	if saved.BlockNumber != 2 {
		t.Fatalf("saved BlockNumber = %d, want 2", saved.BlockNumber)
	}
}

func TestResolveTxInfoUpperBoundByTxLimitAllowsOversizedFirstBlock(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTxCountChain(t, []int{4000, 10})
	upper, txs, oversized := resolveTxInfoUpperBoundByTxLimit(bc, 0, 1, 2000)
	if upper != 1 {
		t.Fatalf("upper = %d, want 1", upper)
	}
	if txs != 4000 {
		t.Fatalf("txs = %d, want 4000", txs)
	}
	if !oversized {
		t.Fatal("oversized = false, want true")
	}
}

func TestResolveTxInfoUpperBoundByTxLimitDefersNextBlock(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTxCountChain(t, []int{1200, 700, 500})
	upper, txs, oversized := resolveTxInfoUpperBoundByTxLimit(bc, 0, 2, 2000)
	if upper != 2 {
		t.Fatalf("upper = %d, want 2", upper)
	}
	if txs != 1900 {
		t.Fatalf("txs = %d, want 1900", txs)
	}
	if oversized {
		t.Fatal("oversized = true, want false")
	}
}

func TestScanTaskRunner_ProcessHeadRespectsConfiguredTxBatchLimit(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTxCountChain(t, []int{1200, 700, 500})
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:      true,
			FromBlock:    0,
			BatchSize:    10,
			BatchTxLimit: 2000,
		},
	})
	task, err := newTxInfoTask(bc, runner.cfg)
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)
	runner.txTask = task
	runner.txStats = newScanTaskCounters("txinfo", task.next)

	txProcessed, _ := runner.processHead(3)
	if txProcessed != 3 {
		t.Fatalf("txProcessed = %d, want 3", txProcessed)
	}
	if runner.txTask.next != 3 {
		t.Fatalf("next = %d, want 3", runner.txTask.next)
	}
}

func TestScanTaskRunner_ProcessHeadUsesTaskSpecificBatchSize(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 20)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		Kyc: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
			BatchSize: 3,
		},
	})
	task, err := newKycTask(bc, runner.cfg)
	if err != nil {
		t.Fatalf("newKycTask error: %v", err)
	}
	t.Cleanup(task.close)
	runner.kycTask = task

	_, kycProcessed := runner.processHead(20)
	if kycProcessed != 3 {
		t.Fatalf("kycProcessed = %d, want 3", kycProcessed)
	}
}

func TestScanTaskCountersSnapshot(t *testing.T) {
	counters := newScanTaskCounters("kyc", 100)
	counters.startedAt = time.Now().Add(-10 * time.Second)
	counters.summaryAt = time.Now().Add(-2 * time.Second)
	counters.summaryBlocks = 12
	counters.recordBatch(100, 124, 149, 500*time.Millisecond)

	snapshot := counters.snapshot(149)
	if snapshot.TotalBlocks != 25 {
		t.Fatalf("TotalBlocks = %d, want 25", snapshot.TotalBlocks)
	}
	if snapshot.RemainingBlocks != 25 {
		t.Fatalf("RemainingBlocks = %d, want 25", snapshot.RemainingBlocks)
	}
	if snapshot.RecentBlocks != 13 {
		t.Fatalf("RecentBlocks = %d, want 13", snapshot.RecentBlocks)
	}
	if snapshot.LastRange != "100-124" {
		t.Fatalf("LastRange = %q, want %q", snapshot.LastRange, "100-124")
	}
	if snapshot.LastBlocksPerSec <= 0 {
		t.Fatalf("LastBlocksPerSec = %f, want > 0", snapshot.LastBlocksPerSec)
	}
	if snapshot.AvgBlocksPerSec <= 0 {
		t.Fatalf("AvgBlocksPerSec = %f, want > 0", snapshot.AvgBlocksPerSec)
	}
}

func TestScanTaskRunner_StopReturnsQuicklyWithBacklog(t *testing.T) {
	bc := newScanTaskTestChain(t, 100000)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	if err := runner.start(); err != nil {
		t.Fatalf("runner.start error: %v", err)
	}
	// 立即 stop，不等待进度，保证 backlog 存在

	start := time.Now()
	runner.stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("runner.stop took too long: %v", elapsed)
	}
}

func TestScanTaskRunner_StopFlushesState(t *testing.T) {
	bc := newScanTaskTestChain(t, 1000)
	out := t.TempDir()
	state := t.TempDir()

	runner := newScanTaskRunner(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     out,
		StateDir:      state,
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	})

	if err := runner.start(); err != nil {
		t.Fatalf("runner.start error: %v", err)
	}
	waitForScanProgress(t, 3*time.Second, func() bool {
		return runner.txTask != nil && runner.txTask.state.BlockNumber >= 10
	})
	runner.stop()

	statePath := filepath.Join(state, "txinfo_state.json")
	blob, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read txinfo state file after stop: %v", err)
	}
	var saved scanTaskState
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("decode txinfo state file: %v", err)
	}
	if saved.BlockNumber < 10 {
		t.Fatalf("saved BlockNumber = %d, want >= 10", saved.BlockNumber)
	}
	if saved.Time == "" {
		t.Fatal("saved Time should not be empty")
	}
}

func TestTxInfoTask_StateFlushesAtBatchEnd(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 6)
	cfg := ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     t.TempDir(),
		StateDir:      t.TempDir(),
		TxInfo: ScanTaskConfig{
			Enabled:   true,
			FromBlock: 0,
		},
	}
	task, err := newTxInfoTask(bc, cfg)
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)

	if err := task.processUntil(bc, 3, nil); err != nil {
		t.Fatalf("processUntil error: %v", err)
	}

	blob, err := os.ReadFile(task.statePath)
	if err != nil {
		t.Fatalf("read state file after batch flush: %v", err)
	}
	var saved scanTaskState
	if err := json.Unmarshal(blob, &saved); err != nil {
		t.Fatalf("decode state file: %v", err)
	}
	if saved.BlockNumber != 3 {
		t.Fatalf("saved BlockNumber = %d, want 3", saved.BlockNumber)
	}
	if saved.Time == "" {
		t.Fatal("saved Time should not be empty")
	}
}

func TestNewTxInfoTask_UsesFixedResultFilePath(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 4)
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 0},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)

	if task.filePath != filepath.Join(outputDir, "txinfo_result.txt") {
		t.Fatalf("filePath = %q, want %q", task.filePath, filepath.Join(outputDir, "txinfo_result.txt"))
	}
	if task.statePath != filepath.Join(stateDir, "txinfo_state.json") {
		t.Fatalf("statePath = %q, want %q", task.statePath, filepath.Join(stateDir, "txinfo_state.json"))
	}
}

func TestTxInfoTask_DoesNotPersistProgressWhenOutputFileClosed(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTxCountChain(t, []int{0, 1})
	outputDir := t.TempDir()
	stateDir := t.TempDir()

	task, err := newTxInfoTask(bc, ScanTasksConfig{
		Confirmations: 0,
		OutputDir:     outputDir,
		StateDir:      stateDir,
		TxInfo:        ScanTaskConfig{Enabled: true, FromBlock: 0},
	})
	if err != nil {
		t.Fatalf("newTxInfoTask error: %v", err)
	}
	t.Cleanup(task.close)

	if err := task.file.Close(); err != nil {
		t.Fatalf("close txinfo file: %v", err)
	}

	if err := task.processUntil(bc, 2, nil); err == nil {
		t.Fatal("processUntil should fail when output file is closed")
	}
	if task.state.BlockNumber != 0 || task.state.BlockHash != "" {
		t.Fatalf("state advanced after closed file error: %+v", task.state)
	}
	if _, hasState, err := loadScanTaskState(task.statePath); err != nil {
		t.Fatalf("loadScanTaskState error: %v", err)
	} else if hasState {
		t.Fatal("state file should not be persisted when output file is closed")
	}
}

func TestScanTaskRunner_DisableTxInfoOnFatalError(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 2)
	out := t.TempDir()
	stateRoot := t.TempDir()
	badStatePath := filepath.Join(stateRoot, "txinfo.state.as.dir")
	if err := os.MkdirAll(badStatePath, 0o755); err != nil {
		t.Fatalf("mkdir bad txinfo state path: %v", err)
	}

	filePath := filepath.Join(out, "txinfo_test.txt")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open txinfo output file: %v", err)
	}

	runner := &scanTaskRunner{
		bc:  bc,
		cfg: ScanTasksConfig{Confirmations: 0},
		txTask: &txInfoTask{
			next:      0,
			state:     scanTaskState{},
			statePath: badStatePath,
			file:      f,
			filePath:  filePath,
		},
	}

	runner.processHead(1)

	if runner.txTask != nil {
		t.Fatal("expected txinfo task to be disabled after fatal error")
	}

	errBlob, err := os.ReadFile(filepath.Join(stateRoot, "txinfo_error.json"))
	if err != nil {
		t.Fatalf("read txinfo error state: %v", err)
	}
	var record scanTaskErrorState
	if err := json.Unmarshal(errBlob, &record); err != nil {
		t.Fatalf("decode txinfo error state: %v", err)
	}
	if record.Task != "txinfo" {
		t.Fatalf("record task = %q, want txinfo", record.Task)
	}
	if record.Error == "" {
		t.Fatal("record error must not be empty")
	}

	output, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read txinfo output file: %v", err)
	}
	if strings.Contains(string(output), "# fatal") {
		t.Fatalf("did not expect fatal marker in output file, got %q", string(output))
	}
}

func TestScanTaskRunner_DisableKycOnFatalError(t *testing.T) {
	t.Parallel()

	bc := newScanTaskTestChain(t, 2)
	out := t.TempDir()
	stateRoot := t.TempDir()
	badStatePath := filepath.Join(stateRoot, "kyc.state.as.dir")
	if err := os.MkdirAll(badStatePath, 0o755); err != nil {
		t.Fatalf("mkdir bad kyc state path: %v", err)
	}

	filePath := filepath.Join(out, "kyc_test.txt")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open kyc output file: %v", err)
	}

	runner := &scanTaskRunner{
		bc:  bc,
		cfg: ScanTasksConfig{Confirmations: 0},
		kycTask: &kycTask{
			next:      0,
			state:     scanTaskState{},
			statePath: badStatePath,
			file:      f,
			filePath:  filePath,
		},
	}

	runner.processHead(1)

	if runner.kycTask != nil {
		t.Fatal("expected kyc task to be disabled after fatal error")
	}

	errBlob, err := os.ReadFile(filepath.Join(stateRoot, "kyc_error.json"))
	if err != nil {
		t.Fatalf("read kyc error state: %v", err)
	}
	var record scanTaskErrorState
	if err := json.Unmarshal(errBlob, &record); err != nil {
		t.Fatalf("decode kyc error state: %v", err)
	}
	if record.Task != "kyc" {
		t.Fatalf("record task = %q, want kyc", record.Task)
	}
	if record.Error == "" {
		t.Fatal("record error must not be empty")
	}

	output, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read kyc output file: %v", err)
	}
	if strings.Contains(string(output), "# fatal") {
		t.Fatalf("did not expect fatal marker in output file, got %q", string(output))
	}
}

func TestSaveScanTaskError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "txinfo_error.json")
	state := scanTaskState{BlockNumber: 12, BlockHash: "0xabc"}
	if err := saveScanTaskError(path, "txinfo", state, errors.New("write failed")); err != nil {
		t.Fatalf("saveScanTaskError failed: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read error file: %v", err)
	}
	var rec scanTaskErrorState
	if err := json.Unmarshal(blob, &rec); err != nil {
		t.Fatalf("decode error file: %v", err)
	}
	if rec.Task != "txinfo" || rec.Error != "write failed" || rec.BlockNumber != 12 || rec.BlockHash != "0xabc" {
		t.Fatalf("unexpected error record: %+v", rec)
	}
}

func TestSaveJSONAtomically_AppendsTrailingNewline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := saveJSONAtomically(path, scanTaskState{BlockNumber: 1, BlockHash: "0x1"}); err != nil {
		t.Fatalf("saveJSONAtomically failed: %v", err)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.HasSuffix(blob, []byte("\n")) {
		t.Fatalf("expected trailing newline, got %q", string(blob))
	}
}

func TestSaveJSONAtomically_PreservesExistingMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod initial file: %v", err)
	}

	if err := saveJSONAtomically(path, scanTaskState{BlockNumber: 1, BlockHash: "0x1"}); err != nil {
		t.Fatalf("saveJSONAtomically failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved file: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("saved file mode = %o, want 640", info.Mode().Perm())
	}
}

func TestRewriteScanTaskOutput_PreservesExistingMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "result.txt")
	content := strings.Join([]string{"keep-1", "drop", "keep-2"}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write result file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod result file: %v", err)
	}

	if err := rewriteScanTaskOutput(path, func(line string) bool {
		return strings.HasPrefix(line, "keep")
	}); err != nil {
		t.Fatalf("rewriteScanTaskOutput failed: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat rewritten file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("rewritten file mode = %o, want 644", info.Mode().Perm())
	}
}

func TestLoadScanTaskStateFailFast_FailsOnCorruptedState(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "txinfo_state.json")
	if err := os.WriteFile(statePath, []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("write corrupted state file: %v", err)
	}

	state, hasState, err := loadScanTaskStateFailFast(statePath, "txinfo")
	if err == nil {
		t.Fatal("loadScanTaskStateFailFast should fail on corrupted state")
	}
	if hasState {
		t.Fatalf("hasState = true, want false")
	}
	if state != (scanTaskState{}) {
		t.Fatalf("state = %+v, want zero value", state)
	}
	if !strings.Contains(err.Error(), "corrupted scan task state file") {
		t.Fatalf("error = %v, want corrupted state message", err)
	}

	errPath := filepath.Join(filepath.Dir(statePath), "txinfo_error.json")
	errBlob, readErr := os.ReadFile(errPath)
	if readErr != nil {
		t.Fatalf("read error state file: %v", readErr)
	}
	var record scanTaskErrorState
	if err := json.Unmarshal(errBlob, &record); err != nil {
		t.Fatalf("decode error state file: %v", err)
	}
	if record.Task != "txinfo" {
		t.Fatalf("record task = %q, want txinfo", record.Task)
	}
	if !strings.Contains(record.Error, "corrupted scan task state file") {
		t.Fatalf("record error = %q, want corrupted-state message", record.Error)
	}
	if _, statErr := os.Stat(statePath); statErr != nil {
		t.Fatalf("original corrupted state file should remain for manual recovery: %v", statErr)
	}
}

func waitForScanProgress(t *testing.T, timeout time.Duration, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for scan task progress")
}
