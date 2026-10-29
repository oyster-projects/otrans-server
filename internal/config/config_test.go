package config

import (
	"strings"
	"testing"
)

func TestLoadRejectsStorageQuotaBelowUploadLimit(t *testing.T) {
	t.Setenv("OTRANS_ADMIN_TOKEN", "test-admin-token-long-enough")
	t.Setenv("OTRANS_MAX_UPLOAD_BYTES", "1024")
	t.Setenv("OTRANS_MAX_STORAGE_BYTES", "512")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "greater than or equal") {
		t.Fatalf("expected quota validation error, got %v", err)
	}
}

func TestLoadUsesSecureStorageDefault(t *testing.T) {
	t.Setenv("OTRANS_ADMIN_TOKEN", "test-admin-token-long-enough")
	t.Setenv("OTRANS_MAX_UPLOAD_BYTES", "")
	t.Setenv("OTRANS_MAX_STORAGE_BYTES", "")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxStorageBytes != defaultMaxStorageBytes || config.MaxStorageBytes < config.MaxUploadBytes {
		t.Fatalf("unexpected storage defaults: upload=%d storage=%d", config.MaxUploadBytes, config.MaxStorageBytes)
	}
}
