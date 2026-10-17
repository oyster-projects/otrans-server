package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultMaxUploadBytes int64 = 4 * 1024 * 1024 * 1024

type Config struct {
	ListenAddr     string
	DataDir        string
	DatabasePath   string
	BlobDir        string
	AdminToken     string
	MaxUploadBytes int64
}

func Load() (Config, error) {
	dataDir := envOrDefault("OTRANS_DATA_DIR", "./data")
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Config{}, err
	}

	adminToken := strings.TrimSpace(os.Getenv("OTRANS_ADMIN_TOKEN"))
	if len(adminToken) < 16 {
		return Config{}, errors.New("OTRANS_ADMIN_TOKEN must contain at least 16 characters")
	}

	maxUploadBytes := defaultMaxUploadBytes
	if raw := strings.TrimSpace(os.Getenv("OTRANS_MAX_UPLOAD_BYTES")); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			return Config{}, errors.New("OTRANS_MAX_UPLOAD_BYTES must be a positive integer")
		}
		maxUploadBytes = parsed
	}

	return Config{
		ListenAddr:     envOrDefault("OTRANS_LISTEN_ADDR", ":8080"),
		DataDir:        dataDir,
		DatabasePath:   filepath.Join(dataDir, "otrans.db"),
		BlobDir:        filepath.Join(dataDir, "blobs"),
		AdminToken:     adminToken,
		MaxUploadBytes: maxUploadBytes,
	}, nil
}

func envOrDefault(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
