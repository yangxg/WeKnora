package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// discoveryCursor is the connector-specific sync state.
//
// Queries maps query_id → candidate external id → fingerprint of the Markdown
// that was ingested. Keying by external id rather than by URL keeps two saved
// queries that happen to find the same page from sharing one slot, which is what
// lets each of them report its own provenance.
//
// The manifest digest is deliberately NOT stored here: it belongs in
// SyncCursor.LastSchemaHash, the field the rest of WeKnora already uses for
// "the configuration this cursor was built under".
type discoveryCursor struct {
	LastSyncTime time.Time                    `json:"last_sync_time"`
	Queries      map[string]map[string]string `json:"queries,omitempty"`
}

func newDiscoveryCursor() *discoveryCursor {
	return &discoveryCursor{
		LastSyncTime: time.Now().UTC(),
		Queries:      make(map[string]map[string]string),
	}
}

// decodeCursor reads the prior connector state, or returns nil when there is
// nothing usable to resume from.
//
// A manifest hash that does not match the current policy discards the state
// instead of trusting it. Skipping is only sound while "already seen" was
// decided under the same policy: after an edit — a widened site list, a longer
// time range — pages the old policy never looked at would stay invisible, and a
// stale cursor is invisible until someone notices missing candidates.
func decodeCursor(ctx context.Context, cursor *types.SyncCursor, manifestHash string) *discoveryCursor {
	if cursor == nil || cursor.ConnectorCursor == nil {
		return nil
	}
	if cursor.LastSchemaHash != manifestHash {
		logger.Infof(ctx, "[Discovery] discovery policy changed since the last sync; re-discovering")
		return nil
	}

	blob, err := json.Marshal(cursor.ConnectorCursor)
	if err != nil {
		logger.Warnf(ctx, "[Discovery] marshal connector cursor: %v", err)
		return nil
	}
	var prev discoveryCursor
	if err := json.Unmarshal(blob, &prev); err != nil {
		logger.Warnf(ctx, "[Discovery] unmarshal connector cursor: %v", err)
		return nil
	}
	return &prev
}

// toSyncCursor wraps the connector state for persistence, stamping the policy it
// was produced under.
func (c *discoveryCursor) toSyncCursor(ctx context.Context, manifestHash string) *types.SyncCursor {
	connectorCursor := make(map[string]interface{})
	if blob, err := json.Marshal(c); err != nil {
		logger.Warnf(ctx, "[Discovery] marshal new cursor: %v", err)
	} else if err := json.Unmarshal(blob, &connectorCursor); err != nil {
		logger.Warnf(ctx, "[Discovery] unmarshal new cursor to map: %v", err)
	}
	return &types.SyncCursor{
		LastSyncTime:    c.LastSyncTime,
		ConnectorCursor: connectorCursor,
		LastSchemaHash:  manifestHash,
	}
}

// carryQueryProgress copies a query's prior state forward.
//
// It is used when a query's *search* failed: that query learned nothing this
// run, so forgetting what it had already ingested would re-emit every one of its
// candidates on recovery. It is deliberately NOT used for a single candidate that
// failed to fetch — that one must stay unrecorded so the next run retries it.
func carryQueryProgress(dst, prev *discoveryCursor, queryID string) {
	if dst == nil || prev == nil {
		return
	}
	if src, ok := prev.Queries[queryID]; ok && len(src) > 0 {
		dst.Queries[queryID] = maps.Clone(src)
	}
}

func priorQueryProgress(prev *discoveryCursor, queryID string) map[string]string {
	if prev == nil {
		return nil
	}
	return prev.Queries[queryID]
}

// contentFingerprint hashes the ingested Markdown so a re-run can tell an
// unchanged page from an edited one.
func contentFingerprint(markdown string) string {
	sum := sha256.Sum256([]byte(markdown))
	return "h:" + hex.EncodeToString(sum[:])[:16]
}
