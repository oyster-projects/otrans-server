package server

import (
	"os"
	"testing"
	"time"
)

func TestRateLimiterRejectsRequestsOverLimit(t *testing.T) {
	limiter := newRateLimiter()
	if allowed, _ := limiter.allow("client", 2, time.Minute); !allowed {
		t.Fatal("first request was rejected")
	}
	if allowed, _ := limiter.allow("client", 2, time.Minute); !allowed {
		t.Fatal("second request was rejected")
	}
	if allowed, _ := limiter.allow("client", 2, time.Minute); allowed {
		t.Fatal("request over limit was accepted")
	}
}

func TestBlobPathsCannotEscapeStorageRoot(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	relative, err := contentAddressPath(digest)
	if err != nil || relative == "" {
		t.Fatalf("valid content path was rejected: %q %v", relative, err)
	}
	if _, err := contentAddressPath("../outside"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Open("../outside"); err == nil {
		t.Fatal("scoped storage root accepted path traversal")
	}
}
