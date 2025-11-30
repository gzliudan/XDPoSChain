package scaner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/log"
)

const idlePollInterval = time.Minute

// ScanTaskConfig describes a single background scan task.
type ScanTaskConfig struct {
	Enabled      bool
	FromBlock    uint64
	BatchSize    uint64
	BatchTxLimit uint64
}

// ScanTasksConfig holds all background scan task settings.
type ScanTasksConfig struct {
	Confirmations uint64
	OutputDir     string `toml:"-"`
	StateDir      string `toml:"-"`
	TxInfo        ScanTaskConfig
	Kyc           ScanTaskConfig
}

type scanTaskRunner struct {
	bc  *core.BlockChain
	cfg ScanTasksConfig

	txTask   *txInfoTask
	kycTask  *kycTask
	txStats  *scanTaskCounters
	kycStats *scanTaskCounters

	quit     chan struct{}
	done     chan struct{}
	startErr error

	startOnce sync.Once
	stopOnce  sync.Once
}

func newScanTaskRunner(bc *core.BlockChain, cfg ScanTasksConfig) *scanTaskRunner {
	return &scanTaskRunner{bc: bc, cfg: cfg}
}

// Runner is the exported scan task runner owned by the eth service lifecycle.
type Runner = scanTaskRunner

// NewRunner creates a new scan task runner instance.
func NewRunner(bc *core.BlockChain, cfg ScanTasksConfig) *Runner {
	return newScanTaskRunner(bc, cfg)
}

// Start launches the configured scan tasks.
func (r *scanTaskRunner) Start() error {
	return r.start()
}

// Stop flushes state and terminates the scan tasks.
func (r *scanTaskRunner) Stop() {
	r.stop()
}

func (r *scanTaskRunner) enabled() bool {
	return r.cfg.TxInfo.Enabled || r.cfg.Kyc.Enabled
}

func resolveTxInfoUpperBoundByTxLimit(bc *core.BlockChain, from, upper, txLimit uint64) (uint64, uint64, bool) {
	if txLimit == 0 || from > upper {
		return upper, 0, false
	}
	boundedUpper := from
	var totalTxs uint64
	oversizedBlock := false

	for blockNumber := from; blockNumber <= upper; blockNumber++ {
		block := bc.GetBlockByNumber(blockNumber)
		if block == nil {
			break
		}
		blockTxs := uint64(len(block.Transactions()))
		if totalTxs > 0 && blockTxs > txLimit-totalTxs {
			break
		}
		boundedUpper = blockNumber
		totalTxs += blockTxs
		if totalTxs > txLimit {
			oversizedBlock = true
			break
		}
	}
	return boundedUpper, totalTxs, oversizedBlock
}

func (r *scanTaskRunner) start() error {
	if !r.enabled() {
		return nil
	}
	if r.startErr != nil {
		return r.startErr
	}
	if r.done != nil {
		return nil
	}
	if r.cfg.OutputDir == "" || r.cfg.StateDir == "" {
		return errorsf("scan task output/state directory is not configured (datadir is required when scan flags are enabled)")
	}
	log.Info("Scan task runner path", "output", r.cfg.OutputDir, "state", r.cfg.StateDir)
	if err := os.MkdirAll(r.cfg.OutputDir, os.ModePerm); err != nil {
		return errorsf("scan task fail to create folder %q: %v", r.cfg.OutputDir, err)
	}
	if err := os.MkdirAll(r.cfg.StateDir, os.ModePerm); err != nil {
		return errorsf("scan task fail to create state folder %q: %v", r.cfg.StateDir, err)
	}
	log.Info("Scan task runner starting", "txinfo", r.cfg.TxInfo.Enabled, "kyc", r.cfg.Kyc.Enabled)

	r.startOnce.Do(func() {
		if r.cfg.TxInfo.Enabled {
			task, err := newTxInfoTask(r.bc, r.cfg)
			if err != nil {
				r.startErr = err
				return
			}
			r.txTask = task
			r.txStats = newScanTaskCounters("txinfo", task.next)
			log.Info("Scan task txinfo initialized",
				"from", task.next,
				"confirmations", r.cfg.Confirmations,
				"batchSize", r.cfg.TxInfo.BatchSize,
				"txBatchLimit", r.cfg.TxInfo.BatchTxLimit,
				"output", task.filePath,
				"state", task.statePath,
			)
		}
		if r.cfg.Kyc.Enabled {
			task, err := newKycTask(r.bc, r.cfg)
			if err != nil {
				if r.txTask != nil {
					r.txTask.close()
					r.txTask = nil
					r.txStats = nil
				}
				r.startErr = err
				return
			}
			r.kycTask = task
			r.kycStats = newScanTaskCounters("kyc", task.next)
			log.Info("Scan task kyc initialized",
				"from", task.next,
				"confirmations", r.cfg.Confirmations,
				"batchSize", r.cfg.Kyc.BatchSize,
				"output", task.filePath,
				"state", task.statePath,
			)
		}

		r.quit = make(chan struct{})
		r.done = make(chan struct{})
		go r.loop()
	})
	if r.startErr != nil {
		return r.startErr
	}
	log.Info("Scan task runner started")
	return nil
}

func (r *scanTaskRunner) stop() {
	r.stopOnce.Do(func() {
		if !r.enabled() || r.done == nil {
			return
		}
		log.Info("Scan task runner is stopping")
		close(r.quit)
		<-r.done
		log.Info("Scan task runner stopped")
	})
}

func (r *scanTaskRunner) loop() {
	defer close(r.done)
	idleTicker := time.NewTicker(idlePollInterval)
	defer idleTicker.Stop()
	backlogActive := false

	defer func() {
		if r.txTask != nil {
			if err := r.txTask.flushState(); err != nil {
				log.Error("Scan task txinfo failed to flush state on stop", "err", err)
			}
			r.txTask.close()
		}
		if r.kycTask != nil {
			if err := r.kycTask.flushState(); err != nil {
				log.Error("Scan task kyc failed to flush state on stop", "err", err)
			}
			r.kycTask.close()
		}
		log.Info("Scan task stopped")
	}()

	for {
		if shouldStopScanTask(r.quit) {
			return
		}
		if r.txTask == nil && r.kycTask == nil {
			return
		}
		if current := r.bc.CurrentBlock(); current != nil {
			headNumber := current.Number.Uint64()
			r.recoverTasks()
			_, ok := confirmedUpperBound(headNumber, r.cfg.Confirmations)
			if ok && r.hasBacklog(headNumber) {
				backlogActive = true
				r.processHead(headNumber)
				continue
			}
			if backlogActive {
				backlogActive = false
			}
		}
		select {
		case <-r.quit:
			return
		case <-idleTicker.C:
		}
	}
}

func (r *scanTaskRunner) hasBacklog(headNumber uint64) bool {
	upper, ok := confirmedUpperBound(headNumber, r.cfg.Confirmations)
	if !ok {
		return false
	}
	if r.txTask != nil && r.txTask.next <= upper {
		return true
	}
	if r.kycTask != nil && r.kycTask.next <= upper {
		return true
	}
	return false
}

func (r *scanTaskRunner) processHead(headNumber uint64) (uint64, uint64) {
	var txProcessed uint64
	var kycProcessed uint64
	r.recoverTasks()
	upper, ok := confirmedUpperBound(headNumber, r.cfg.Confirmations)
	if !ok {
		return 0, 0
	}
	if r.txTask != nil {
		if r.txTask.next <= upper {
			from := r.txTask.next
			txUpper := upper
			if r.txTask.next <= txUpper {
				maxUpper := r.txTask.next + r.cfg.TxInfo.BatchSize - 1
				if maxUpper < txUpper {
					txUpper = maxUpper
				}
			}
			batchTxs := uint64(0)
			oversizedBlock := false
			txLimit := r.cfg.TxInfo.BatchTxLimit
			if from <= txUpper {
				txUpper, batchTxs, oversizedBlock = resolveTxInfoUpperBoundByTxLimit(r.bc, from, txUpper, txLimit)
			}
			startedAt := time.Now()
			if err := r.txTask.processUntil(r.bc, txUpper, r.quit); err != nil {
				r.txTask.recordFatalError(err)
				r.txTask.close()
				r.txTask = nil
				log.Error("Scan task txinfo disabled after fatal error", "err", err)
			} else if r.txTask.next > from {
				txProcessed = r.txTask.next - from
				snapshot := r.txStats.recordBatch(from, r.txTask.next-1, upper, time.Since(startedAt))
				batchTxsPerSec := roundedBlocksPerSecond(batchTxs, snapshot.LastElapsed)
				log.Info("Scan task txinfo batch", "from", from, "to", r.txTask.next-1, "confirmedHead", upper, "batchBlocks", txProcessed, "batchTxs", batchTxs, "batchTxsPerSec", batchTxsPerSec, "remainingBlocks", snapshot.RemainingBlocks, "elapsed", snapshot.LastElapsed, "oversizedBlock", oversizedBlock)
			}
		}
	}
	if r.kycTask != nil {
		if r.kycTask.next <= upper {
			from := r.kycTask.next
			kycUpper := upper
			if r.kycTask.next <= kycUpper {
				maxUpper := r.kycTask.next + r.cfg.Kyc.BatchSize - 1
				if maxUpper < kycUpper {
					kycUpper = maxUpper
				}
			}
			startedAt := time.Now()
			if err := r.kycTask.processUntil(r.bc, kycUpper, r.quit); err != nil {
				r.kycTask.recordFatalError(err)
				r.kycTask.close()
				r.kycTask = nil
				log.Error("Scan task kyc disabled after fatal error", "err", err)
			} else if r.kycTask.next > from {
				kycProcessed = r.kycTask.next - from
				snapshot := r.kycStats.recordBatch(from, r.kycTask.next-1, upper, time.Since(startedAt))
				log.Info("Scan task kyc batch", "from", from, "to", r.kycTask.next-1, "confirmedHead", upper, "batchBlocks", kycProcessed, "batchBlocksPerSec", snapshot.LastBlocksPerSec, "remainingBlocks", snapshot.RemainingBlocks, "elapsed", snapshot.LastElapsed)
			}
		}
	}
	return txProcessed, kycProcessed
}

func (r *scanTaskRunner) recoverTasks() {
	if r.txTask != nil {
		rewound, err := r.txTask.rewindToCanonical(r.bc)
		if err != nil {
			r.txTask.recordFatalError(err)
			r.txTask.close()
			r.txTask = nil
			r.txStats = nil
			log.Error("Scan task txinfo disabled after rewind failure", "err", err)
		} else if rewound {
			r.txStats = newScanTaskCounters("txinfo", r.txTask.next)
		}
	}
	if r.kycTask != nil {
		rewound, err := r.kycTask.rewindToCanonical(r.bc)
		if err != nil {
			r.kycTask.recordFatalError(err)
			r.kycTask.close()
			r.kycTask = nil
			r.kycStats = nil
			log.Error("Scan task kyc disabled after rewind failure", "err", err)
		} else if rewound {
			r.kycStats = newScanTaskCounters("kyc", r.kycTask.next)
		}
	}
}

func confirmedUpperBound(headNumber, confirmations uint64) (uint64, bool) {
	if headNumber < confirmations {
		return 0, false
	}
	return headNumber - confirmations, true
}

func shouldStopScanTask(quit <-chan struct{}) bool {
	if quit == nil {
		return false
	}
	select {
	case <-quit:
		return true
	default:
		return false
	}
}

type scanTaskSnapshot struct {
	Task               string
	StartBlock         uint64
	NextBlock          uint64
	TotalBlocks        uint64
	RecentBlocks       uint64
	RemainingBlocks    uint64
	Batches            uint64
	Elapsed            time.Duration
	RecentElapsed      time.Duration
	LastElapsed        time.Duration
	LastBlocksPerSec   float64
	RecentBlocksPerSec float64
	AvgBlocksPerSec    float64
	LastRange          string
}

type scanTaskCounters struct {
	task          string
	startBlock    uint64
	nextBlock     uint64
	startedAt     time.Time
	summaryAt     time.Time
	totalBlocks   uint64
	summaryBlocks uint64
	batches       uint64
	lastFrom      uint64
	lastTo        uint64
	lastElapsed   time.Duration
	lastRate      float64
	hasLast       bool
}

func newScanTaskCounters(task string, startBlock uint64) *scanTaskCounters {
	now := time.Now()
	return &scanTaskCounters{task: task, startBlock: startBlock, nextBlock: startBlock, startedAt: now, summaryAt: now}
}

func (c *scanTaskCounters) recordBatch(from, to, upper uint64, elapsed time.Duration) scanTaskSnapshot {
	if c == nil {
		return scanTaskSnapshot{}
	}
	processed := uint64(0)
	if to >= from {
		processed = to - from + 1
		c.lastFrom = from
		c.lastTo = to
		c.nextBlock = to + 1
		c.hasLast = true
	}
	c.totalBlocks += processed
	c.batches++
	c.lastElapsed = elapsed
	c.lastRate = roundedBlocksPerSecond(processed, elapsed)
	return c.snapshot(upper)
}

func (c *scanTaskCounters) snapshot(upper uint64) scanTaskSnapshot {
	if c == nil {
		return scanTaskSnapshot{}
	}
	now := time.Now()
	remaining := uint64(0)
	if c.nextBlock <= upper {
		remaining = upper - c.nextBlock + 1
	}
	recentBlocks := c.totalBlocks - c.summaryBlocks
	lastRange := ""
	if c.hasLast {
		lastRange = fmt.Sprintf("%d-%d", c.lastFrom, c.lastTo)
	}
	return scanTaskSnapshot{
		Task:               c.task,
		StartBlock:         c.startBlock,
		NextBlock:          c.nextBlock,
		TotalBlocks:        c.totalBlocks,
		RecentBlocks:       recentBlocks,
		RemainingBlocks:    remaining,
		Batches:            c.batches,
		Elapsed:            now.Sub(c.startedAt),
		RecentElapsed:      now.Sub(c.summaryAt),
		LastElapsed:        c.lastElapsed,
		LastBlocksPerSec:   c.lastRate,
		RecentBlocksPerSec: roundedBlocksPerSecond(recentBlocks, now.Sub(c.summaryAt)),
		AvgBlocksPerSec:    roundedBlocksPerSecond(c.totalBlocks, now.Sub(c.startedAt)),
		LastRange:          lastRange,
	}
}

func roundedBlocksPerSecond(blocks uint64, elapsed time.Duration) float64 {
	if blocks == 0 || elapsed <= 0 {
		return 0
	}
	rate := float64(blocks) / elapsed.Seconds()
	return math.Round(rate*100) / 100
}

type scanTaskState struct {
	BlockNumber uint64 `json:"block_number"`
	BlockHash   string `json:"block_hash"`
	StartBlock  uint64 `json:"start_block,omitempty"`
	Time        string `json:"time"`
}

type scanTaskErrorState struct {
	Task        string `json:"task"`
	Time        string `json:"time"`
	Error       string `json:"error"`
	BlockNumber uint64 `json:"block_number"`
	BlockHash   string `json:"block_hash"`
}

type scanTaskResumePlan struct {
	Next        uint64
	ResetOutput bool
	State       scanTaskState
	HasState    bool
}

func resolveNextBlock(bc *core.BlockChain, fromBlock uint64, state scanTaskState, hasState bool, taskName string) scanTaskResumePlan {
	previousStart := state.StartBlock
	state.StartBlock = fromBlock
	if !hasState || state.BlockNumber < fromBlock {
		return scanTaskResumePlan{Next: fromBlock, State: state, HasState: hasState}
	}
	if previousStart != 0 && fromBlock < previousStart {
		safeState, hasSafeState := canonicalScanTaskStateAt(bc, fromBlock)
		safeState.StartBlock = fromBlock
		log.Warn("Scan task configured start block moved backward, rewinding", "task", taskName, "savedStart", previousStart, "from", fromBlock)
		return scanTaskResumePlan{Next: fromBlock, ResetOutput: true, State: safeState, HasState: hasSafeState}
	}
	block := bc.GetBlockByNumber(state.BlockNumber)
	if block == nil || !strings.EqualFold(block.Hash().Hex(), state.BlockHash) {
		rollback := resolveScanTaskRollback(bc, fromBlock, state)
		safeState, hasSafeState := canonicalScanTaskStateAt(bc, rollback)
		safeState.StartBlock = fromBlock
		log.Warn("Scan task state mismatch on canonical chain, rewinding", "task", taskName, "blockNumber", state.BlockNumber, "from", rollback)
		return scanTaskResumePlan{Next: rollback, ResetOutput: true, State: safeState, HasState: hasSafeState}
	}
	return scanTaskResumePlan{Next: state.BlockNumber + 1, State: state, HasState: true}
}

func resolveScanTaskRollback(bc *core.BlockChain, fromBlock uint64, state scanTaskState) uint64 {
	rollback := fromBlock
	if savedHash := common.HexToHash(state.BlockHash); savedHash != (common.Hash{}) {
		if savedHeader := bc.GetHeader(savedHash, state.BlockNumber); savedHeader != nil {
			if currentHead := bc.CurrentHeader(); currentHead != nil {
				if ancestor := findCommonAncestorHeader(bc, savedHeader, currentHead); ancestor != nil {
					if next := ancestor.Number.Uint64() + 1; next > rollback {
						rollback = next
					}
				}
			}
		}
	}
	if current := bc.CurrentBlock(); current != nil {
		headNext := current.Number.Uint64()
		if headNext < math.MaxUint64 {
			headNext++
		}
		if rollback > headNext {
			rollback = headNext
		}
	}
	return rollback
}

func findCommonAncestorHeader(bc *core.BlockChain, a, b *types.Header) *types.Header {
	if a == nil || b == nil {
		return nil
	}
	for a.Number.Uint64() > b.Number.Uint64() {
		a = bc.GetHeader(a.ParentHash, a.Number.Uint64()-1)
		if a == nil {
			return nil
		}
	}
	for b.Number.Uint64() > a.Number.Uint64() {
		b = bc.GetHeader(b.ParentHash, b.Number.Uint64()-1)
		if b == nil {
			return nil
		}
	}
	for a.Hash() != b.Hash() {
		a = bc.GetHeader(a.ParentHash, a.Number.Uint64()-1)
		if a == nil {
			return nil
		}
		b = bc.GetHeader(b.ParentHash, b.Number.Uint64()-1)
		if b == nil {
			return nil
		}
	}
	return a
}

func canonicalScanTaskStateAt(bc *core.BlockChain, next uint64) (scanTaskState, bool) {
	if next == 0 {
		return scanTaskState{}, false
	}
	block := bc.GetBlockByNumber(next - 1)
	if block == nil {
		return scanTaskState{}, false
	}
	return scanTaskState{BlockNumber: next - 1, BlockHash: block.Hash().Hex()}, true
}

func resolveMissingScanTaskOutput(bc *core.BlockChain, filePath string, fromBlock uint64, plan scanTaskResumePlan, taskName string, blockNumber func(string) (uint64, bool)) (scanTaskResumePlan, error) {
	lastBlock, exists, err := scanTaskLastOutputBlock(filePath, blockNumber)
	statePath := ""
	stateBlock := uint64(0)
	stateHash := ""
	if plan.State.BlockNumber != 0 || plan.State.BlockHash != "" {
		stateBlock = plan.State.BlockNumber
		stateHash = plan.State.BlockHash
	}
	// Try to infer the state file path for logging
	if strings.Contains(filePath, "kyc_result.txt") {
		statePath = filepath.Join(filepath.Dir(filePath), "state", "kyc_state.json")
	} else if strings.Contains(filePath, "txinfo_result.txt") {
		statePath = filepath.Join(filepath.Dir(filePath), "state", "txinfo_state.json")
	}
	reason := ""
	if err != nil {
		reason = err.Error()
		// Fix: For KYC task, treat result file with only header as a normal empty file, do not trigger rewind or warning.
		if taskName == "kyc" && strings.Contains(reason, "only_header") {
			return plan, nil
		}
		log.Warn("Scan task output check failed, rewinding to rebuild history", "task", taskName, "file", filePath, "statePath", statePath, "from", fromBlock, "resume", plan.Next, "stateBlock", stateBlock, "stateHash", stateHash, "reason", reason)
		safeState, hasSafeState := canonicalScanTaskStateAt(bc, fromBlock)
		return scanTaskResumePlan{Next: fromBlock, ResetOutput: true, State: safeState, HasState: hasSafeState}, nil
	}
	if !exists || lastBlock+1 < plan.Next {
		if reason == "" {
			if !exists {
				reason = "no_valid_data"
			} else {
				reason = fmt.Sprintf("lagging last_block=%d", lastBlock)
			}
		}
		safeState, hasSafeState := canonicalScanTaskStateAt(bc, fromBlock)
		log.Warn("Scan task output is incomplete, rewinding to rebuild history", "task", taskName, "file", filePath, "fileExists", exists, "lastBlockInFile", lastBlock, "from", fromBlock, "resume", plan.Next, "statePath", statePath, "stateBlock", stateBlock, "stateHash", stateHash, "reason", reason)
		return scanTaskResumePlan{Next: fromBlock, ResetOutput: true, State: safeState, HasState: hasSafeState}, nil
	}
	return plan, nil
}

func scanTaskLastOutputBlock(path string, blockNumber func(string) (uint64, bool)) (uint64, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		log.Error("Scan task last output block: os.Stat failed", "path", path, "err", err)
		if os.IsNotExist(err) {
			return 0, false, fmt.Errorf("not_exist")
		}
		return 0, false, fmt.Errorf("stat_error: %v", err)
	}
	if info.IsDir() {
		log.Error("Scan task last output block: path is a directory, not a file", "path", path)
		return 0, false, fmt.Errorf("is_directory")
	}

	source, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("open_error: %v", err)
	}
	defer source.Close()

	var (
		lastBlock uint64
		found     bool
		lineCount int
	)
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++
		block, ok := blockNumber(line)
		if ok {
			lastBlock = block
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, false, fmt.Errorf("scan_error: %v", err)
	}
	if lineCount == 0 {
		return 0, false, fmt.Errorf("empty_file")
	}
	if lineCount == 1 && !found {
		return 0, false, fmt.Errorf("only_header")
	}
	if !found {
		return 0, false, fmt.Errorf("no_valid_data")
	}
	return lastBlock, true, nil
}

func syncScanTaskState(path string, state scanTaskState, hasState bool) error {
	if !hasState {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return saveScanTaskState(path, state)
}

func scanTaskOutputNeedsRewrite(path string, blockNumber func(string) (uint64, bool), rollback uint64) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.IsDir() {
		return false, fmt.Errorf("scan task result path %q is a directory", path)
	}

	source, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer source.Close()

	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		block, ok := blockNumber(scanner.Text())
		if ok && block >= rollback {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func rewriteScanTaskOutput(path string, shouldKeep func(string) bool) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("scan task result path %q is a directory", path)
	}

	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".scan-output-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(info.Mode()); err != nil {
		return err
	}

	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !shouldKeep(line) {
			continue
		}
		if _, err := tmp.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func loadScanTaskState(path string) (scanTaskState, bool, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return scanTaskState{}, false, nil
		}
		return scanTaskState{}, false, err
	}
	if len(blob) == 0 {
		return scanTaskState{}, false, nil
	}
	var state scanTaskState
	if err := json.Unmarshal(blob, &state); err != nil {
		return scanTaskState{}, false, err
	}
	return state, true, nil
}

func isCorruptedScanTaskStateError(err error) bool {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

func loadScanTaskStateFailFast(path, taskName string) (scanTaskState, bool, error) {
	state, hasState, err := loadScanTaskState(path)
	if err == nil {
		return state, hasState, nil
	}
	if !isCorruptedScanTaskStateError(err) {
		return scanTaskState{}, false, err
	}
	errPath := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s_error.json", taskName))
	cause := fmt.Errorf("corrupted scan task state file %q: %w", path, err)
	if saveErr := saveScanTaskError(errPath, taskName, scanTaskState{}, cause); saveErr != nil {
		log.Error("Scan task failed to persist startup error state", "name", taskName, "path", errPath, "err", saveErr)
	}
	log.Error("Scan task fail to startup due to corrupted state file", "name", taskName, "path", path, "errorState", errPath, "err", err)
	return scanTaskState{}, false, cause
}

func saveScanTaskState(path string, state scanTaskState) error {
	state.Time = time.Now().Format("2006-01-02 15:04:05")
	return saveJSONAtomically(path, state)
}

func saveScanTaskError(path, taskName string, state scanTaskState, cause error) error {
	record := scanTaskErrorState{
		Task:        taskName,
		Time:        time.Now().Format("2006-01-02 15:04:05"),
		Error:       cause.Error(),
		BlockNumber: state.BlockNumber,
		BlockHash:   state.BlockHash,
	}
	return saveJSONAtomically(path, record)
}

func saveJSONAtomically(path string, value interface{}) error {
	blob, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	blob = append(blob, '\n')
	dir := filepath.Dir(path)
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	tmp, err := os.CreateTemp(dir, ".scan-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func errorsf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}
