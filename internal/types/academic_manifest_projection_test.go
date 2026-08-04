package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The academic lane is a second discovery lane with the opposite answer to the
// only question that matters: its candidates carry no body. That difference is
// why it is a separate connector type rather than a flag on the existing one
// (ResearchFlow ADR-0012 §5, ADR-0013 §6). Test-only; the two constants under
// test are the contract, and this file pins the shape ResearchFlow materializes.

// academicSettings mirrors one materialized academic manifest, as the decoded
// JSON a connector reads back rather than as Go structs.
func academicSettings() map[string]interface{} {
	return map[string]interface{}{
		"project_id":       "medical-policy",
		"source_id":        "src-medical-academic",
		"manifest_hash":    strings.Repeat("c", 64),
		"provider_type":    "pubmed",
		"endpoint_profile": "api_key",
		// The positive statement. There is no need_content key to set to false:
		// in this lane a body-related setting is not "off", it is inapplicable.
		"identity_only": true,
		"count":         25,
		"date_range":    "2024-01-01..2024-12-31",
		"work_types":    []interface{}{"journal-article", "review"},
		"open_access":   "any",
		// Non-secret registered identity: NCBI requires it, it is not a
		// credential, and a reviewer must be able to see what the project tells
		// the registry about itself (ADR-0012 §5 gap 2).
		"contact": map[string]interface{}{"email": "research-flow@example.org", "tool": "ResearchFlow"},
		"queries": []interface{}{
			map[string]interface{}{"query_id": "drg-payment-reform", "query": "DRG payment reform"},
		},
	}
}

// TestAcademicConnectorTypeAndChannelAreDistinct pins that the academic lane is
// not spelled like the web one.
//
// Sharing "discovery" for both would put candidates that carry a body and
// candidates that carry none behind one runtime value, and every downstream
// reader — provenance, review, promotion — would have to re-derive which lane a
// row came from. The two lanes differ in what their candidates legally are, so
// they differ here.
func TestAcademicConnectorTypeAndChannelAreDistinct(t *testing.T) {
	assert.Equal(t, "academic", ConnectorTypeAcademic)
	assert.Equal(t, "academic", ChannelAcademic)
	assert.NotEqual(t, ConnectorTypeDiscovery, ConnectorTypeAcademic,
		"one connector type for both lanes would hide two different body rules behind one value")
	assert.NotEqual(t, ChannelDiscovery, ChannelAcademic,
		"provenance must say which lane a candidate came from")
}

// TestAcademicManifestProjection_SettingsStayReadableCredentialsDoNot mirrors the
// web lane's split: policy diffable in clear, credential unreadable at rest.
func TestAcademicManifestProjection_SettingsStayReadableCredentialsDoNot(t *testing.T) {
	withAESKey(t, testAESKey32)
	const apiKey = "ncbi-academic-key-do-not-log"
	cfg := &DataSourceConfig{
		Type:        ConnectorTypeAcademic,
		Credentials: map[string]interface{}{"api_key": apiKey},
		Settings:    academicSettings(),
	}

	blob, err := cfg.ToJSON()
	assert.NoError(t, err)
	assert.NotContains(t, string(blob), apiKey, "the registry key must not be legible at rest")
	assert.Contains(t, string(blob), "src-medical-academic", "policy stays diffable against the manifest")

	var raw map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal(blob, &raw))
	var creds map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw["credentials"], &creds))
	assert.True(t, strings.HasPrefix(creds["api_key"].(string), "enc:v1:"))

	var settings map[string]interface{}
	assert.NoError(t, json.Unmarshal(raw["settings"], &settings))
	assert.NotContains(t, settings, "secret_ref",
		"the credential reference belongs to the manifest, not the materialized row")

	// The contact is non-secret by decision and must stay legible: hiding it in
	// the encrypted half would defeat the reason it is in the manifest at all.
	contact, ok := settings["contact"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, "research-flow@example.org", contact["email"])
}

// TestAcademicManifestProjection_NoSettingCanRequestABody is the load-bearing
// one. ADR-0012 §2 forbids this lane from producing governed text, and ADR-0013
// §2 makes the candidate's card the mechanism that keeps WeKnora from fetching a
// landing page. Both depend on the materialized row never carrying a key that a
// connector could read as "also bring the content".
func TestAcademicManifestProjection_NoSettingCanRequestABody(t *testing.T) {
	withAESKey(t, testAESKey32)
	cfg := &DataSourceConfig{
		Type:        ConnectorTypeAcademic,
		Credentials: map[string]interface{}{"api_key": "ncbi-academic-key"},
		Settings:    academicSettings(),
	}
	blob, err := cfg.ToJSON()
	assert.NoError(t, err)
	parsed, err := (&DataSource{Config: blob}).ParseConfig()
	assert.NoError(t, err)

	for _, banned := range []string{
		"need_content", "need_url", "search_type", "content_formats",
		"abstract", "summary", "snippet", "fulltext", "industry", "sites",
	} {
		assert.NotContains(t, parsed.Settings, banned,
			"%s is a web-lane concept; its presence here would let a connector treat "+
				"identity-only candidates as body-bearing", banned)
	}

	// A bool that came back as the string "true" would read as truthy either way,
	// so the type is asserted rather than the truthiness.
	assert.Equal(t, true, parsed.Settings["identity_only"],
		"identity_only must survive as a bool the connector can branch on")
	assert.Equal(t, "ncbi-academic-key", parsed.Credentials["api_key"],
		"the connector receives the decrypted key, never the ciphertext")
}
