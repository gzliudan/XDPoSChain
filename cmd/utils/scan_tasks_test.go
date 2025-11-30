// Copyright 2026 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package utils

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/eth/scaner"
	"github.com/XinFinOrg/XDPoSChain/node"
	"github.com/urfave/cli/v2"
)

func TestSetScanTasksRemainsDisabledWhenFlagUnset(t *testing.T) {
	t.Parallel()

	datadir := t.TempDir()
	stack, err := node.New(&node.Config{DataDir: datadir})
	if err != nil {
		t.Fatalf("node.New error: %v", err)
	}
	defer stack.Close()

	set := flagSetWithScanTasks(t, nil)
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := scaner.ScanTasksConfig{
		OutputDir:     filepath.Join(datadir, "custom-scan-output"),
		StateDir:      filepath.Join(datadir, "custom-scan-output", "state"),
		Confirmations: 77,
		TxInfo: scaner.ScanTaskConfig{
			Enabled:   true,
			FromBlock: 12,
		},
		Kyc: scaner.ScanTaskConfig{
			Enabled:   true,
			FromBlock: 34,
		},
	}

	SetScanTasks(ctx, stack, &cfg)

	if cfg.Confirmations != 77 {
		t.Fatalf("Confirmations = %d, want 77", cfg.Confirmations)
	}
	if cfg.TxInfo.Enabled || cfg.TxInfo.FromBlock != 0 || cfg.TxInfo.BatchSize != 0 || cfg.TxInfo.BatchTxLimit != 0 {
		t.Fatalf("TxInfo = %+v, want disabled zero-value config when CLI flag is absent", cfg.TxInfo)
	}
	if cfg.Kyc.Enabled || cfg.Kyc.FromBlock != 0 || cfg.Kyc.BatchSize != 0 {
		t.Fatalf("Kyc = %+v, want disabled zero-value config when CLI flag is absent", cfg.Kyc)
	}
	if cfg.OutputDir != "" {
		t.Fatalf("OutputDir = %q, want empty when CLI flag is absent", cfg.OutputDir)
	}
	if cfg.StateDir != "" {
		t.Fatalf("StateDir = %q, want empty when CLI flag is absent", cfg.StateDir)
	}
}

func TestSetScanTasksFlagOverridesConfig(t *testing.T) {
	t.Parallel()

	datadir := t.TempDir()
	stack, err := node.New(&node.Config{DataDir: datadir})
	if err != nil {
		t.Fatalf("node.New error: %v", err)
	}
	defer stack.Close()

	set := flagSetWithScanTasks(t, []string{"--scan-txinfo=9:321:456"})
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := scaner.ScanTasksConfig{
		Confirmations: 77,
		TxInfo: scaner.ScanTaskConfig{
			Enabled:      true,
			FromBlock:    12,
			BatchSize:    100,
			BatchTxLimit: 1000,
		},
		Kyc: scaner.ScanTaskConfig{
			Enabled:   true,
			FromBlock: 34,
			BatchSize: 200,
		},
	}

	SetScanTasks(ctx, stack, &cfg)

	if cfg.Confirmations != 77 {
		t.Fatalf("Confirmations = %d, want 77", cfg.Confirmations)
	}
	if !cfg.TxInfo.Enabled || cfg.TxInfo.FromBlock != 9 || cfg.TxInfo.BatchSize != 321 || cfg.TxInfo.BatchTxLimit != 456 {
		t.Fatalf("TxInfo = %+v, want enabled from 9 with batch 321 and tx limit 456", cfg.TxInfo)
	}
	if cfg.Kyc.Enabled {
		t.Fatalf("Kyc = %+v, want disabled", cfg.Kyc)
	}
	if cfg.OutputDir != filepath.Join(datadir, "scan-tasks") {
		t.Fatalf("OutputDir = %q, want %q", cfg.OutputDir, filepath.Join(datadir, "scan-tasks"))
	}
	if cfg.StateDir != filepath.Join(datadir, "scan-tasks", "state") {
		t.Fatalf("StateDir = %q, want %q", cfg.StateDir, filepath.Join(datadir, "scan-tasks", "state"))
	}
}

func TestSetScanTasksUsesEmbeddedDefaults(t *testing.T) {
	t.Parallel()

	datadir := t.TempDir()
	stack, err := node.New(&node.Config{DataDir: datadir})
	if err != nil {
		t.Fatalf("node.New error: %v", err)
	}
	defer stack.Close()

	set := flagSetWithScanTasks(t, []string{"--scan-txinfo=:1000", "--scan-kyc=5:"})
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := scaner.ScanTasksConfig{Confirmations: 77}

	SetScanTasks(ctx, stack, &cfg)

	if !cfg.TxInfo.Enabled || cfg.TxInfo.FromBlock != 1 || cfg.TxInfo.BatchSize != 1000 || cfg.TxInfo.BatchTxLimit != 20000 {
		t.Fatalf("TxInfo = %+v, want enabled from 1 with batch 1000 and tx limit 20000", cfg.TxInfo)
	}
	if !cfg.Kyc.Enabled || cfg.Kyc.FromBlock != 5 || cfg.Kyc.BatchSize != 10000 {
		t.Fatalf("Kyc = %+v, want enabled from 5 with default batch 10000", cfg.Kyc)
	}
}

func TestSetScanTasksEmptyFlagUsesDefaults(t *testing.T) {
	t.Parallel()

	datadir := t.TempDir()
	stack, err := node.New(&node.Config{DataDir: datadir})
	if err != nil {
		t.Fatalf("node.New error: %v", err)
	}
	defer stack.Close()

	set := flagSetWithScanTasks(t, []string{"--scan-txinfo=", "--scan-kyc="})
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := scaner.ScanTasksConfig{Confirmations: 77}

	SetScanTasks(ctx, stack, &cfg)

	if !cfg.TxInfo.Enabled || cfg.TxInfo.FromBlock != 1 || cfg.TxInfo.BatchSize != 5000 || cfg.TxInfo.BatchTxLimit != 100000 {
		t.Fatalf("TxInfo = %+v, want enabled with CLI-only defaults (batchSize=5000, batchTxLimit=100000)", cfg.TxInfo)
	}
	if !cfg.Kyc.Enabled || cfg.Kyc.FromBlock != 1 || cfg.Kyc.BatchSize != 10000 {
		t.Fatalf("Kyc = %+v, want enabled with defaults", cfg.Kyc)
	}
}

func TestSetScanTasksIgnoresPreloadedConfigWhenFlagSet(t *testing.T) {
	t.Parallel()

	datadir := t.TempDir()
	stack, err := node.New(&node.Config{DataDir: datadir})
	if err != nil {
		t.Fatalf("node.New error: %v", err)
	}
	defer stack.Close()

	set := flagSetWithScanTasks(t, []string{"--scan-txinfo=", "--scan-kyc="})
	ctx := cli.NewContext(cli.NewApp(), set, nil)
	cfg := scaner.ScanTasksConfig{
		Confirmations: 77,
		TxInfo: scaner.ScanTaskConfig{
			Enabled:      true,
			FromBlock:    12,
			BatchSize:    321,
			BatchTxLimit: 654,
		},
		Kyc: scaner.ScanTaskConfig{
			Enabled:   true,
			FromBlock: 34,
			BatchSize: 222,
		},
	}

	SetScanTasks(ctx, stack, &cfg)

	if !cfg.TxInfo.Enabled || cfg.TxInfo.FromBlock != 1 || cfg.TxInfo.BatchSize != 5000 || cfg.TxInfo.BatchTxLimit != 100000 {
		t.Fatalf("TxInfo = %+v, want CLI-only defaults (batchSize=5000, batchTxLimit=100000)", cfg.TxInfo)
	}
	if !cfg.Kyc.Enabled || cfg.Kyc.FromBlock != 1 || cfg.Kyc.BatchSize != 10000 {
		t.Fatalf("Kyc = %+v, want CLI-only defaults (batchSize=10000)", cfg.Kyc)
	}
}

func flagSetWithScanTasks(t *testing.T, args []string) *flag.FlagSet {
	t.Helper()

	set := flag.NewFlagSet("test", flag.ContinueOnError)
	if err := ScanTxInfoFlag.Apply(set); err != nil {
		t.Fatalf("ScanTxInfoFlag.Apply error: %v", err)
	}
	if err := ScanKycFlag.Apply(set); err != nil {
		t.Fatalf("ScanKycFlag.Apply error: %v", err)
	}
	if err := set.Parse(args); err != nil {
		t.Fatalf("flag parse error: %v", err)
	}
	return set
}
