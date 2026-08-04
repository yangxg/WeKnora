// Package querycursor holds the sync state shared by the saved-query connectors.
//
// Two lanes run saved queries and emit candidates: web discovery, whose
// candidates carry the original page, and academic discovery, whose candidates
// carry identity only. They differ in what a candidate *is* — which is why they
// are separate connectors (ResearchFlow ADR-0012 §6) — but they keep progress
// the same way: per saved query, per candidate, a fingerprint of whatever was
// ingested, discarded wholesale when the policy that produced it changed.
//
// That state machine is subtle in one specific place, and the subtlety is the
// reason this is a package rather than duplicated code: the three failure
// granularities a sync can have are not interchangeable, and each is expressed
// by a *different* operation here.
//
//   - One candidate failed → simply do not Record it. The next run retries it.
//   - One query's search failed → CarryQueryProgress, so what that query had
//     already emitted is not re-emitted on recovery.
//   - Every query failed → the caller returns no cursor at all. Nothing in this
//     package can express that, and it must not: the sync service persists any
//     non-nil cursor even on a fetch error, so a cursor returned after a total
//     outage would mark every candidate as seen.
//
// The manifest digest is deliberately not stored in the blob: it belongs in
// SyncCursor.LastSchemaHash, the field the rest of WeKnora already uses for "the
// configuration this cursor was built under".
package querycursor

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

// Cursor is the connector-specific sync state.
//
// Queries maps query_id → candidate external id → fingerprint of what was
// ingested. Keying by external id rather than by URL or identity keeps two saved
// queries that happen to find the same work from sharing one slot, which is what
// lets each of them report its own provenance.
type Cursor struct {
	LastSyncTime time.Time                    `json:"last_sync_time"`
	Queries      map[string]map[string]string `json:"queries,omitempty"`
}

// New returns an empty cursor stamped with the current time.
func New() *Cursor {
	return &Cursor{
		LastSyncTime: time.Now().UTC(),
		Queries:      make(map[string]map[string]string),
	}
}

// Decode reads the prior connector state, or returns nil when there is nothing
// usable to resume from. lane names the connector in log lines.
//
// A manifest hash that does not match the current policy discards the state
// instead of trusting it. Skipping is only sound while "already seen" was decided
// under the same policy: after an edit — a widened site list, a longer date range,
// another registry — candidates the old policy never looked at would stay
// invisible, and a stale cursor is invisible until someone notices what is
// missing.
func Decode(ctx context.Context, cursor *types.SyncCursor, manifestHash, lane string) *Cursor {
	if cursor == nil || cursor.ConnectorCursor == nil {
		return nil
	}
	if cursor.LastSchemaHash != manifestHash {
		logger.Infof(ctx, "[%s] search policy changed since the last sync; re-discovering", lane)
		return nil
	}

	blob, err := json.Marshal(cursor.ConnectorCursor)
	if err != nil {
		logger.Warnf(ctx, "[%s] marshal connector cursor: %v", lane, err)
		return nil
	}
	var prev Cursor
	if err := json.Unmarshal(blob, &prev); err != nil {
		logger.Warnf(ctx, "[%s] unmarshal connector cursor: %v", lane, err)
		return nil
	}
	return &prev
}

// ToSyncCursor wraps the connector state for persistence, stamping the policy it
// was produced under.
func (c *Cursor) ToSyncCursor(ctx context.Context, manifestHash, lane string) *types.SyncCursor {
	connectorCursor := make(map[string]interface{})
	if blob, err := json.Marshal(c); err != nil {
		logger.Warnf(ctx, "[%s] marshal new cursor: %v", lane, err)
	} else if err := json.Unmarshal(blob, &connectorCursor); err != nil {
		logger.Warnf(ctx, "[%s] unmarshal new cursor to map: %v", lane, err)
	}
	return &types.SyncCursor{
		LastSyncTime:    c.LastSyncTime,
		ConnectorCursor: connectorCursor,
		LastSchemaHash:  manifestHash,
	}
}

// StartQuery opens a query's slot in this run's state.
//
// It is called once a query's search has *succeeded*, which is what makes an
// empty slot meaningful: it says "this query ran and found nothing new", as
// distinct from a query whose search failed and whose prior progress was carried
// forward instead.
func (c *Cursor) StartQuery(queryID string) {
	if c == nil {
		return
	}
	if c.Queries == nil {
		c.Queries = make(map[string]map[string]string)
	}
	c.Queries[queryID] = make(map[string]string)
}

// Record marks one candidate as ingested under the given fingerprint.
//
// A candidate that failed must NOT be recorded: recording it would mark an unread
// item as ingested and it would never be retried.
func (c *Cursor) Record(queryID, externalID, fingerprint string) {
	if c == nil {
		return
	}
	if c.Queries == nil {
		c.Queries = make(map[string]map[string]string)
	}
	if c.Queries[queryID] == nil {
		c.Queries[queryID] = make(map[string]string)
	}
	c.Queries[queryID][externalID] = fingerprint
}

// CarryQueryProgress copies a query's prior state forward.
//
// It is used when a query's *search* failed: that query learned nothing this run,
// so forgetting what it had already ingested would re-emit every one of its
// candidates on recovery. It is deliberately NOT used for a single candidate that
// failed — that one must stay unrecorded so the next run retries it.
//
// The copy is a clone: sharing the map with the prior cursor would let this run's
// Record calls mutate the state a caller may still be reading from.
func (c *Cursor) CarryQueryProgress(prev *Cursor, queryID string) {
	if c == nil || prev == nil {
		return
	}
	if src, ok := prev.Queries[queryID]; ok && len(src) > 0 {
		if c.Queries == nil {
			c.Queries = make(map[string]map[string]string)
		}
		c.Queries[queryID] = maps.Clone(src)
	}
}

// PriorProgress returns what a query had already ingested, or nil.
func PriorProgress(prev *Cursor, queryID string) map[string]string {
	if prev == nil {
		return nil
	}
	return prev.Queries[queryID]
}

// Fingerprint hashes what a connector ingested so a re-run can tell an unchanged
// candidate from a changed one.
//
// It is deliberately content-agnostic: the web lane hashes the Markdown of the
// original page, the academic lane hashes the bibliographic card it renders. Both
// only need the value to be stable for unchanged input — which is why the
// academic lane's card must render deterministically, or every run would look
// like it found new results.
func Fingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "h:" + hex.EncodeToString(sum[:])[:16]
}
