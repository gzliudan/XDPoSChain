package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_RejectsScanTasksSection(t *testing.T) {
	t.Parallel()

	configFile := filepath.Join(t.TempDir(), "XDC.toml")
	content := `
[ScanTasks]
Confirmations = 7
BatchSize = 123
BatchTxLimit = 456

[ScanTasks.TxInfo]
Enabled = true
FromBlock = 9
BatchSize = 12
BatchTxLimit = 34
`
	if err := os.WriteFile(configFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg := XDCConfig{}
	err := loadConfig(configFile, &cfg)
	if err == nil {
		t.Fatal("expected loadConfig to reject the unsupported [ScanTasks] section")
	}
	if !strings.Contains(err.Error(), "ScanTasks") {
		t.Fatalf("error = %v, want ScanTasks-related error", err)
	}
}
