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
	t.Setenv("OTRANS_RETENTION_DAYS", "")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxStorageBytes != defaultMaxStorageBytes || config.MaxStorageBytes < config.MaxUploadBytes {
		t.Fatalf("unexpected storage defaults: upload=%d storage=%d", config.MaxUploadBytes, config.MaxStorageBytes)
	}
	if config.RetentionDays != defaultRetentionDays {
		t.Fatalf("unexpected retention default: %d", config.RetentionDays)
	}
}

func TestLoadAllowsDisablingAutomaticDeletion(t *testing.T) {
	t.Setenv("OTRANS_ADMIN_TOKEN", "test-admin-token-long-enough")
	t.Setenv("OTRANS_RETENTION_DAYS", "0")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.RetentionDays != 0 {
		t.Fatalf("expected disabled retention, got %d", config.RetentionDays)
	}
}

func TestLoadRejectsInvalidRetentionDays(t *testing.T) {
	for _, value := range []string{"-1", "36501", "not-a-number"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OTRANS_ADMIN_TOKEN", "test-admin-token-long-enough")
			t.Setenv("OTRANS_RETENTION_DAYS", value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "OTRANS_RETENTION_DAYS") {
				t.Fatalf("expected retention validation error for %q, got %v", value, err)
			}
		})
	}
}
