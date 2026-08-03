package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ResearchFlow keeps discovery policy in a version-controlled project manifest
// and materializes it onto a data source row: the policy travels in Settings,
// the vendor key travels in Credentials, and the manifest digest travels in the
// sync cursor. None of that needs a new field — but all three properties are
// load-bearing, so they are pinned here rather than assumed.
//
// See ResearchFlow ADR-0011. Test-only; no production symbol is touched.

// discoverySettings mirrors one materialized manifest. Values are the decoded
// JSON shapes a connector would read back, not Go structs, because that is what
// survives the DB round trip.
func discoverySettings() map[string]interface{} {
	return map[string]interface{}{
		"project_id":       "medical-policy",
		"source_id":        "src-medical-discovery",
		"manifest_hash":    strings.Repeat("a", 64),
		"provider_type":    "volcengine",
		"endpoint_profile": "api_key",
		"search_type":      "web",
		"need_content":     false,
		"need_url":         true,
		"count":            20,
		"time_range":       "OneMonth",
		"content_formats":  []interface{}{"markdown"},
		"query_rewrite":    false,
		"auth_info_level":  []interface{}{1, 2},
		"sites":            []interface{}{"www.gov.cn", "www.nhc.gov.cn"},
		"block_hosts":      []interface{}{"news.aggregator.example"},
		"queries": []interface{}{
			map[string]interface{}{"query_id": "drg-payment-reform", "query": "DRG 付费方式改革"},
		},
	}
}

// TestDiscoveryManifestProjection_SettingsStayReadableCredentialsDoNot pins the
// split the manifest contract depends on: an agent must be able to diff the
// materialized row against the manifest it came from, so Settings has to stay
// in clear; the vendor key must never be legible at rest, so Credentials has to
// be encrypted. Both halves of that live in one Config blob, which is exactly
// why it is worth asserting they behave differently.
func TestDiscoveryManifestProjection_SettingsStayReadableCredentialsDoNot(t *testing.T) {
	withAESKey(t, testAESKey32)
	const apiKey = "volc-discovery-key-do-not-log"
	cfg := &DataSourceConfig{
		Type:        "discovery",
		Credentials: map[string]interface{}{"api_key": apiKey},
		Settings:    discoverySettings(),
	}

	blob, err := cfg.ToJSON()
	assert.NoError(t, err)
	serialized := string(blob)

	assert.NotContains(t, serialized, apiKey, "the vendor key must not be legible at rest")
	assert.Contains(t, serialized, "src-medical-discovery", "policy stays diffable against the manifest")
	assert.Contains(t, serialized, "DRG 付费方式改革", "saved queries are config state, not audit text")

	var raw map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(blob, &raw))
	var creds map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw["credentials"], &creds))
	assert.True(t, strings.HasPrefix(creds["api_key"].(string), "enc:v1:"))

	var settings map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw["settings"], &settings))
	for key := range settings {
		value, isString := settings[key].(string)
		if isString {
			assert.False(t, strings.HasPrefix(value, "enc:v1:"),
				"settings[%s] must stay in clear: encrypting policy would make the row undiffable", key)
		}
	}
	assert.NotContains(t, settings, "secret_ref",
		"the credential reference belongs to the manifest, not the materialized row")
}

// TestDiscoveryManifestProjection_ProfileSurvivesTheRoundTripTyped pins that the
// three fields fixing the discovery profile come back as the types a connector
// branches on. A bool that returns as the string "false" would read as truthy in
// a careless connector and start requesting vendor page bodies, which is the one
// thing the profile exists to prevent.
func TestDiscoveryManifestProjection_ProfileSurvivesTheRoundTripTyped(t *testing.T) {
	withAESKey(t, testAESKey32)
	cfg := &DataSourceConfig{
		Type:        "discovery",
		Credentials: map[string]interface{}{"api_key": "volc-discovery-key"},
		Settings:    discoverySettings(),
	}
	blob, err := cfg.ToJSON()
	assert.NoError(t, err)

	parsed, err := (&DataSource{Config: blob}).ParseConfig()
	assert.NoError(t, err)
	assert.NotNil(t, parsed)

	assert.Equal(t, "web", parsed.Settings["search_type"])
	assert.Equal(t, false, parsed.Settings["need_content"])
	assert.Equal(t, true, parsed.Settings["need_url"])
	assert.EqualValues(t, 20, parsed.Settings["count"])
	assert.Equal(t, "volc-discovery-key", parsed.Credentials["api_key"],
		"the connector receives the decrypted key, never the ciphertext")

	queries, ok := parsed.Settings["queries"].([]interface{})
	assert.True(t, ok, "saved queries must survive as a list the connector can iterate")
	assert.Len(t, queries, 1)
	assert.Equal(t, "drg-payment-reform", queries[0].(map[string]interface{})["query_id"])
}

// TestDiscoveryManifestProjection_CursorCarriesTheManifestHash pins the field
// ResearchFlow uses to notice that a manifest changed. LastSchemaHash is
// declared upstream but unreferenced, so its JSON key is asserted by name: a
// rename would silently stop resetting cursors on a policy edit, and stale
// cursors are invisible until someone notices missing candidates.
func TestDiscoveryManifestProjection_CursorCarriesTheManifestHash(t *testing.T) {
	manifestHash := strings.Repeat("b", 64)
	cursor := &SyncCursor{
		LastSchemaHash:  manifestHash,
		ConnectorCursor: map[string]interface{}{"drg-payment-reform": "2026-08-01T00:00:00Z"},
	}

	blob, err := cursor.ToJSON()
	assert.NoError(t, err)
	assert.Contains(t, string(blob), `"last_schema_hash"`)

	parsed, err := (&DataSource{LastSyncCursor: blob}).ParseSyncCursor()
	assert.NoError(t, err)
	assert.Equal(t, manifestHash, parsed.LastSchemaHash)
	assert.Equal(t, "2026-08-01T00:00:00Z", parsed.ConnectorCursor["drg-payment-reform"])
}

// TestDiscoveryManifestProjection_DeterministicIDIsPreserved pins what makes
// re-materialization idempotent: ResearchFlow derives the row id from
// project_id + source_id, so creating with that id must not have it replaced.
// Without this, a repeated materialize would fork a second data source and the
// same queries would run twice into one inbox.
func TestDiscoveryManifestProjection_DeterministicIDIsPreserved(t *testing.T) {
	const deterministicID = "2f8d7b16-9d4a-5d0e-8c3a-1b6f0e5a7c92"

	supplied := &DataSource{ID: deterministicID}
	assert.NoError(t, supplied.BeforeCreate(nil))
	assert.Equal(t, deterministicID, supplied.ID)

	generated := &DataSource{}
	assert.NoError(t, generated.BeforeCreate(nil))
	assert.NotEmpty(t, generated.ID, "an absent id is still filled in for every other caller")
}
