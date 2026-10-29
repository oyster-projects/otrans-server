package server

import (
	"context"
	"time"
)

const staleDraftTTL = 48 * time.Hour

func (s *Server) RunMaintenance(ctx context.Context) {
	s.runMaintenance(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runMaintenance(ctx)
		}
	}
}

func (s *Server) runMaintenance(ctx context.Context) {
	if err := s.store.CleanupExpiredPairingCodes(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("failed to remove expired pairing codes", "error", err)
	}
	digests, err := s.store.DeleteStaleDrafts(ctx, time.Now().UTC().Add(-staleDraftTTL))
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("failed to remove stale draft transfers", "error", err)
		}
		return
	}
	for _, digest := range digests {
		if err := s.removeBlobIfUnused(ctx, digest); err != nil && ctx.Err() == nil {
			s.log.Error("failed to remove stale draft blob", "digest", digest, "error", err)
		}
	}
}
