package scaner

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/log"
)

const DefaultTxinfoBatchSize uint64 = 5000
const DefaultTxinfoTxLimit uint64 = DefaultTxinfoBatchSize * 20

type txInfoTask struct {
	next       uint64
	startBlock uint64
	state      scanTaskState
	statePath  string
	dirty      bool
	file       *os.File
	filePath   string
}

func newTxInfoTask(bc *core.BlockChain, cfg ScanTasksConfig) (*txInfoTask, error) {
	statePath := filepath.Join(cfg.StateDir, "txinfo_state.json")
	state, hasState, err := loadScanTaskStateFailFast(statePath, "txinfo")
	if err != nil {
		return nil, err
	}

	filePath := filepath.Join(cfg.OutputDir, "txinfo_result.txt")
	plan := resolveNextBlock(bc, cfg.TxInfo.FromBlock, state, hasState, "txinfo")
	plan, err = resolveMissingScanTaskOutput(bc, filePath, cfg.TxInfo.FromBlock, plan, "txinfo", parseTxInfoResultBlockNumber)
	if err != nil {
		return nil, err
	}
	needsRewrite := plan.ResetOutput
	if !needsRewrite {
		needsRewrite, err = scanTaskOutputNeedsRewrite(filePath, parseTxInfoResultBlockNumber, plan.Next)
		if err != nil {
			return nil, err
		}
	}
	if needsRewrite {
		if err := truncateTxInfoResultFile(filePath, plan.Next); err != nil {
			return nil, err
		}
	}
	if plan.ResetOutput {
		state = plan.State
		hasState = plan.HasState
		if err := syncScanTaskState(statePath, state, hasState); err != nil {
			return nil, err
		}
		log.Warn("Scan task txinfo truncated result file after rewind", "file", filePath, "from", plan.Next)
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	log.Info("Scan task txinfo open result file", "file", filePath, "from", plan.Next)
	return &txInfoTask{next: plan.Next, startBlock: cfg.TxInfo.FromBlock, state: state, statePath: statePath, file: f, filePath: filePath}, nil
}

func (t *txInfoTask) rewindToCanonical(bc *core.BlockChain) (bool, error) {
	hasState := t.state.BlockHash != ""
	plan := resolveNextBlock(bc, t.startBlock, t.state, hasState, "txinfo")
	if !plan.ResetOutput {
		return false, nil
	}
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	if err := truncateTxInfoResultFile(t.filePath, plan.Next); err != nil {
		return false, err
	}
	if err := syncScanTaskState(t.statePath, plan.State, plan.HasState); err != nil {
		return false, err
	}
	f, err := os.OpenFile(t.filePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	t.file = f
	t.next = plan.Next
	t.state = plan.State
	t.dirty = false
	log.Warn("Scan task txinfo rewound during runtime", "from", plan.Next, "file", t.filePath)
	return true, nil
}

func truncateTxInfoResultFile(path string, rollback uint64) error {
	return rewriteScanTaskOutput(path, func(line string) bool {
		blockNumber, ok := parseTxInfoResultBlockNumber(line)
		return ok && blockNumber < rollback
	})
}

func parseTxInfoResultBlockNumber(line string) (uint64, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	blockNumber, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return blockNumber, true
}

func (t *txInfoTask) processUntil(bc *core.BlockChain, upper uint64, quit <-chan struct{}) (err error) {
	builder := strings.Builder{}
	var (
		processedAny bool
		lastNumber   uint64
		lastHash     string
	)
	defer func() {
		if !processedAny {
			return
		}
		if t.file != nil {
			if syncErr := t.file.Sync(); syncErr != nil {
				if err == nil {
					err = syncErr
				} else {
					log.Error("Scan task txinfo failed to sync result file", "block", lastNumber, "err", syncErr)
				}
			}
		}
		if err != nil {
			return
		}
		t.state.BlockNumber = lastNumber
		t.state.BlockHash = lastHash
		t.dirty = true
		if flushErr := t.flushState(); flushErr != nil {
			if err == nil {
				err = flushErr
			} else {
				log.Error("Scan task txinfo failed to persist progress state", "block", lastNumber, "err", flushErr)
			}
		}
	}()

	for t.next <= upper {
		if shouldStopScanTask(quit) {
			return nil
		}
		block := bc.GetBlockByNumber(t.next)
		if block == nil {
			break
		}
		for _, tx := range block.Transactions() {
			to := tx.To()
			if to != nil {
				fmt.Fprintf(&builder, "%s %s %d\n", tx.Hash().Hex(), to.String0x(), t.next)
			} else {
				fmt.Fprintf(&builder, "%s n %d\n", tx.Hash().Hex(), t.next)
			}
		}
		if builder.Len() > 0 {
			if _, err := t.file.WriteString(builder.String()); err != nil {
				return err
			}
			builder.Reset()
		}
		processedAny = true
		lastNumber = t.next
		lastHash = block.Hash().Hex()
		t.next++
	}
	return nil
}

func (t *txInfoTask) flushState() error {
	if !t.dirty {
		return nil
	}
	if err := saveScanTaskState(t.statePath, t.state); err != nil {
		return err
	}
	t.dirty = false
	return nil
}

func (t *txInfoTask) close() {
	if t.file != nil {
		log.Info("Scan task txinfo closing result file", "name", t.filePath)
		t.file.Close()
		t.file = nil
	}
}

func (t *txInfoTask) recordFatalError(cause error) {
	if err := t.flushState(); err != nil {
		log.Error("Scan task txinfo failed to persist progress state before fatal error", "path", t.statePath, "err", err)
	}
	errorPath := filepath.Join(filepath.Dir(t.statePath), "txinfo_error.json")
	if err := saveScanTaskError(errorPath, "txinfo", t.state, cause); err != nil {
		log.Error("Scan task txinfo failed to persist error state", "path", errorPath, "err", err)
	}
}
