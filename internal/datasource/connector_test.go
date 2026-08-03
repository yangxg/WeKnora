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

func TestFeishuMetadataDoesNotAdvertiseWebhook(t *testing.T) {
	meta := ConnectorMetadataRegistry[types.ConnectorTypeFeishu]

	for _, capability := range meta.Capabilities {
		if capability == "webhook" {
			t.Fatalf("Feishu connector should not advertise webhook until webhook sync is implemented")
		}
	}
}
