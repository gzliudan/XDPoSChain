package scaner

import (
	"bytes"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/params"
)

const DefaultKycBatchSize uint64 = 10000

type kycTask struct {
	next       uint64
	startBlock uint64
	state      scanTaskState
	statePath  string
	dirty      bool
	file       *os.File
	filePath   string
}

func newKycTask(bc *core.BlockChain, cfg ScanTasksConfig) (*kycTask, error) {
	const headerLine = "block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx"
	statePath := filepath.Join(cfg.StateDir, "kyc_state.json")
	state, hasState, err := loadScanTaskStateFailFast(statePath, "kyc")
	if err != nil {
		return nil, err
	}

	filePath := filepath.Join(cfg.OutputDir, "kyc_result.txt")
	plan := resolveNextBlock(bc, cfg.Kyc.FromBlock, state, hasState, "kyc")
	// Only trust the state file for progress; do not trigger ResetOutput if the result file is missing or incomplete
	var lastBlockInFile uint64
	var fileExists bool
	planFromFile, err := resolveMissingScanTaskOutput(bc, filePath, cfg.Kyc.FromBlock, plan, "kyc", func(line string) (uint64, bool) {
		return parseKycResultBlockNumber(line, headerLine)
	})
	if err != nil {
		log.Warn("Scan task kyc output file check failed, ignored, only trusting state file for progress", "file", filePath, "err", err)
	} else {
		lastBlockInFile, fileExists, _ = scanTaskLastOutputBlock(filePath, func(line string) (uint64, bool) {
			return parseKycResultBlockNumber(line, headerLine)
		})
		if fileExists && lastBlockInFile >= plan.Next {
			// Output file has stale tail, forcibly truncate to plan.Next, keep state as plan.State
			if err := truncateKycResultFile(filePath, plan.Next, headerLine); err != nil {
				return nil, err
			}
			state = plan.State
			hasState = plan.HasState
			if err := syncScanTaskState(statePath, state, hasState); err != nil {
				return nil, err
			}
			log.Warn("Scan task kyc output file stale tail, truncated to plan.Next, progress from state", "file", filePath, "from", plan.Next)
		} else if planFromFile.ResetOutput {
			// If the result file is behind state (lastBlockInFile < plan.Next), just continue from state, do NOT rewind.
			if !fileExists || lastBlockInFile < plan.Next {
				log.Info("Scan task kyc output file is behind state, will continue from state without rewind", "file", filePath, "lastBlockInFile", lastBlockInFile, "stateBlock", plan.Next)
				// No rewind, just continue
			} else {
				// For other cases (e.g. disorder, corruption), still rewind as before
				if err := truncateKycResultFile(filePath, cfg.Kyc.FromBlock, headerLine); err != nil {
					return nil, err
				}
				safeState, hasSafeState := canonicalScanTaskStateAt(bc, cfg.Kyc.FromBlock)
				state = safeState
				hasState = hasSafeState
				plan.Next = cfg.Kyc.FromBlock
				plan.State = safeState
				plan.HasState = hasSafeState
				if err := syncScanTaskState(statePath, state, hasState); err != nil {
					return nil, err
				}
				log.Warn("Scan task kyc output file incomplete, truncated and rewound to fromBlock", "file", filePath, "from", cfg.Kyc.FromBlock)
			}
		}
	}
	// Only trigger ResetOutput on chain reorg or state file inconsistency
	needsRewrite := plan.ResetOutput
	if needsRewrite {
		if err := truncateKycResultFile(filePath, plan.Next, headerLine); err != nil {
			return nil, err
		}
		state = plan.State
		hasState = plan.HasState
		if err := syncScanTaskState(statePath, state, hasState); err != nil {
			return nil, err
		}
		log.Warn("Scan task kyc truncated result file after rewind", "file", filePath, "from", plan.Next)
	}

	newFile := false
	if info, statErr := os.Stat(filePath); statErr != nil {
		if os.IsNotExist(statErr) {
			newFile = true
		} else {
			return nil, statErr
		}
	} else if info.IsDir() {
		return nil, fmt.Errorf("Scan task kyc result path %q is a directory", filePath)
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if newFile {
		if _, err = f.WriteString(headerLine + "\n"); err != nil {
			f.Close()
			return nil, err
		}
	}
	log.Info("Scan task kyc open result file", "name", filePath, "from", plan.Next)
	return &kycTask{next: plan.Next, startBlock: cfg.Kyc.FromBlock, state: state, statePath: statePath, file: f, filePath: filePath}, nil
}

func (t *kycTask) rewindToCanonical(bc *core.BlockChain) (bool, error) {
	hasState := t.state.BlockHash != ""
	plan := resolveNextBlock(bc, t.startBlock, t.state, hasState, "kyc")
	// Only trust the state file for progress; do not trigger ResetOutput if the result file is missing or incomplete
	if !plan.ResetOutput {
		return false, nil
	}
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
	const headerLine = "block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx"
	if err := truncateKycResultFile(t.filePath, plan.Next, headerLine); err != nil {
		return false, err
	}
	if err := syncScanTaskState(t.statePath, plan.State, plan.HasState); err != nil {
		return false, err
	}
	f, err := os.OpenFile(t.filePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return false, err
	}
	if info, err := os.Stat(t.filePath); err == nil && info.Size() == 0 {
		if _, err := f.WriteString(headerLine + "\n"); err != nil {
			f.Close()
			return false, err
		}
	}
	t.file = f
	t.next = plan.Next
	t.state = plan.State
	t.dirty = false
	log.Warn("Scan task kyc rewound during runtime, only trusting state file for progress", "from", plan.Next, "file", t.filePath)
	return true, nil
}

func truncateKycResultFile(path string, rollback uint64, headerLine string) error {
	return rewriteScanTaskOutput(path, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == headerLine {
			return true
		}
		// Exclude all comment lines (starting with #)
		if strings.HasPrefix(trimmed, "#") {
			// DEBUG: forcibly exclude all comment lines
			return false
		}
		blockNumber, ok := parseKycResultBlockNumber(trimmed, headerLine)
		return ok && blockNumber < rollback
	})
}

func parseKycResultBlockNumber(line string, headerLine string) (uint64, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed == headerLine {
		return 0, false
	}
	if strings.HasPrefix(trimmed, "# warning ") {
		return parseScanTaskCommentBlockNumber(trimmed)
	}
	if strings.HasPrefix(trimmed, "#") {
		return 0, false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return 0, false
	}
	blockNumber, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return blockNumber, true
}

func parseScanTaskCommentBlockNumber(line string) (uint64, bool) {
	idx := strings.Index(line, "block=")
	if idx == -1 {
		return 0, false
	}
	raw := line[idx+len("block="):]
	end := strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' })
	if end >= 0 {
		raw = raw[:end]
	}
	if raw == "" {
		return 0, false
	}
	blockNumber, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return blockNumber, true
}

func (t *kycTask) processUntil(bc *core.BlockChain, upper uint64, quit <-chan struct{}) (err error) {
	voteInvalidKYCSelector := common.FromHex("0xf2ee3c7d")
	invalidatedNodeTopic := common.HexToHash("0xe18d61a5bf4aa2ab40afc88aa9039d27ae17ff4ec1c65f5f414df6f02ce8b35e")
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
					log.Error("Scan task kyc failed to sync result file", "block", lastNumber, "err", syncErr)
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
				log.Error("Scan task kyc failed to persist progress state", "block", lastNumber, "err", flushErr)
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
		blockHash := block.Hash().Hex()
		if !ownerFeeRoutingActive(block.Number(), bc.Config()) {
			processedAny = true
			lastNumber = t.next
			lastHash = blockHash
			t.next++
			continue
		}

		txs := block.Transactions()
		matchIndexes, hasLaterNormal := collectVoteInvalidKYCTransactions(txs, voteInvalidKYCSelector)
		if len(matchIndexes) == 0 {
			processedAny = true
			lastNumber = t.next
			lastHash = blockHash
			t.next++
			continue
		}

		receipts := bc.GetReceiptsByHash(block.Hash())
		if len(receipts) != len(txs) {
			return fmt.Errorf("incomplete receipts for kyc scan at block %d: txs=%d receipts=%d", t.next, len(txs), len(receipts))
		}

		var (
			coinbaseCandidate     common.Address
			haveCoinbaseCandidate bool
		)
		for pos, txIndex := range matchIndexes {
			tx := txs[txIndex]
			hasLaterNormalTx := hasLaterNormal[pos]

			txSucceeded := false
			invalidated := false
			changesCoinbaseOwner := false
			if txIndex < len(receipts) && receipts[txIndex] != nil {
				txSucceeded = receipts[txIndex].Status == types.ReceiptStatusSuccessful
				if txSucceeded {
					if !haveCoinbaseCandidate {
						coinbaseCandidate, err = bc.Engine().Author(block.Header())
						if err != nil {
							coinbaseCandidate = block.Coinbase()
						}
						haveCoinbaseCandidate = true
					}
					invalidated, changesCoinbaseOwner = classifyVoteInvalidKYCOwnerChange(receipts[txIndex].Logs, coinbaseCandidate, invalidatedNodeTopic)
				}
			}

			fmt.Fprintf(&builder, "%d %s %d %s voteInvalidKYC %t %t %t %t\n", t.next, blockHash, txIndex, tx.Hash().Hex(), txSucceeded, invalidated, changesCoinbaseOwner, hasLaterNormalTx)
		}

		if builder.Len() > 0 {
			if _, err := t.file.WriteString(builder.String()); err != nil {
				return err
			}
			builder.Reset()
		}
		processedAny = true
		lastNumber = t.next
		lastHash = blockHash
		t.next++
	}

	return nil
}

func collectVoteInvalidKYCTransactions(txs types.Transactions, voteInvalidKYCSelector []byte) ([]int, []bool) {
	var matchIndexes []int
	for i, tx := range txs {
		to := tx.To()
		if to == nil || *to != common.MasternodeVotingSMCBinary {
			continue
		}
		data := tx.Data()
		if len(data) < len(voteInvalidKYCSelector) {
			continue
		}
		if !bytes.Equal(data[:len(voteInvalidKYCSelector)], voteInvalidKYCSelector) {
			continue
		}
		matchIndexes = append(matchIndexes, i)
	}
	if len(matchIndexes) == 0 {
		return nil, nil
	}

	hasLaterByIndex := make([]bool, len(txs))
	seenNormal := false
	for i := len(txs) - 1; i >= 0; i-- {
		hasLaterByIndex[i] = seenNormal
		if !txs[i].IsSpecialTransaction() {
			seenNormal = true
		}
	}
	hasLaterNormal := make([]bool, len(matchIndexes))
	for i, txIndex := range matchIndexes {
		hasLaterNormal[i] = hasLaterByIndex[txIndex]
	}
	return matchIndexes, hasLaterNormal
}

func ownerFeeRoutingActive(blockNumber *big.Int, chainConfig *params.ChainConfig) bool {
	return blockNumber.Cmp(chainConfig.TIPTRC21FeeBlock) > 0
}

func (t *kycTask) flushState() error {
	if !t.dirty {
		return nil
	}
	if err := saveScanTaskState(t.statePath, t.state); err != nil {
		return err
	}
	t.dirty = false
	return nil
}

func (t *kycTask) close() {
	if t.file != nil {
		log.Info("Scan task kyc closing result file", "name", t.filePath)
		t.file.Close()
		t.file = nil
	}
}

func (t *kycTask) recordFatalError(cause error) {
	if err := t.flushState(); err != nil {
		log.Error("Scan task kyc failed to persist progress state before fatal error", "path", t.statePath, "err", err)
	}
	errorPath := filepath.Join(filepath.Dir(t.statePath), "kyc_error.json")
	if err := saveScanTaskError(errorPath, "kyc", t.state, cause); err != nil {
		log.Error("Scan task kyc failed to persist error state", "path", errorPath, "err", err)
	}
}

func classifyVoteInvalidKYCOwnerChange(logs []*types.Log, coinbaseCandidate common.Address, invalidatedNodeTopic common.Hash) (bool, bool) {
	for _, receiptLog := range logs {
		if receiptLog.Address == common.MasternodeVotingSMCBinary && len(receiptLog.Topics) > 0 && receiptLog.Topics[0] == invalidatedNodeTopic {
			if invalidatedNodeIncludesCandidate(receiptLog.Data, coinbaseCandidate) {
				return true, true
			}
			return true, false
		}
	}
	return false, false
}

func invalidatedNodeIncludesCandidate(data []byte, candidate common.Address) bool {
	if len(data) < 96 {
		return false
	}
	dataLen := uint64(len(data))
	offset := new(big.Int).SetBytes(data[32:64]).Uint64()
	if offset > dataLen || offset > dataLen-32 {
		return false
	}
	arrayStart := offset
	length := new(big.Int).SetBytes(data[arrayStart : arrayStart+32]).Uint64()
	elementsStart := arrayStart + 32
	available := dataLen - elementsStart
	if length > available/32 {
		return false
	}
	for index := uint64(0); index < length; index++ {
		start := elementsStart + index*32
		addr := common.BytesToAddress(data[start+12 : start+32])
		if addr == candidate {
			return true
		}
	}
	return false
}
