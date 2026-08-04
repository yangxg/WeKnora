package datasource

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// TestDiscoveryMetadataDoesNotAdvertiseDeletionSync pins a claim the discovery
// connector cannot honour: a hit leaving a search vendor's ranking says nothing
// about the page behind it, so there is no deletion signal to sync. Advertising
// one would invite a caller to treat a shrinking result set as removals.
func TestDiscoveryMetadataDoesNotAdvertiseDeletionSync(t *testing.T) {
	meta := ConnectorMetadataRegistry[types.ConnectorTypeDiscovery]
	if meta.Type != types.ConnectorTypeDiscovery {
		t.Fatal("discovery connector metadata is missing")
	}
	for _, capability := range meta.Capabilities {
		if capability == "deletion_sync" {
			t.Fatal("discovery has no deletion signal to advertise")
		}
	}
}

// TestAcademicMetadataAdvertisesOnlyIncremental pins the connector's lifecycle
// claim. A work dropping out of a registry's result set is not a retraction or a
// deletion, and removing its bibliographic card would erase review state based
// on ranking movement alone (ResearchFlow ADR-0013 §6).
func TestAcademicMetadataAdvertisesOnlyIncremental(t *testing.T) {
	meta := ConnectorMetadataRegistry[types.ConnectorTypeAcademic]
	if meta.Type != types.ConnectorTypeAcademic {
		t.Fatal("academic connector metadata is missing")
	}
	if len(meta.Capabilities) != 1 || meta.Capabilities[0] != "incremental" {
		t.Fatalf("academic capabilities = %v, want incremental only", meta.Capabilities)
	}
	if meta.AuthType != "mixed" {
		t.Fatalf("academic auth type = %q, want mixed for anonymous/key/contact provider profiles", meta.AuthType)
	}
}

func TestFeishuMetadataDoesNotAdvertiseWebhook(t *testing.T) {
	meta := ConnectorMetadataRegistry[types.ConnectorTypeFeishu]

	for _, capability := range meta.Capabilities {
		if capability == "webhook" {
			t.Fatalf("Feishu connector should not advertise webhook until webhook sync is implemented")
		}
	}
}
