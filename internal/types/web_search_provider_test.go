package types

import "testing"

func TestGetWebSearchProviderTypesIncludesZhipuConfig(t *testing.T) {
	var zhipu *WebSearchProviderTypeInfo
	providerTypes := GetWebSearchProviderTypes()
	for i := range providerTypes {
		if providerTypes[i].ID == string(WebSearchProviderTypeZhipu) {
			zhipu = &providerTypes[i]
			break
		}
	}
	if zhipu == nil {
		t.Fatal("Zhipu provider metadata is missing")
	}
	if !zhipu.RequiresAPIKey || !zhipu.SupportsProxy {
		t.Fatalf("unexpected Zhipu capability metadata: %+v", zhipu)
	}
	if len(zhipu.ConfigFields) != 2 {
		t.Fatalf("len(ConfigFields) = %d, want 2", len(zhipu.ConfigFields))
	}
	if zhipu.ConfigFields[0].Key != "search_engine" || zhipu.ConfigFields[0].Default != "search_std" {
		t.Fatalf("unexpected search engine config metadata: %+v", zhipu.ConfigFields[0])
	}
	if len(zhipu.ConfigFields[0].Options) != 4 {
		t.Fatalf("len(search engine options) = %d, want 4", len(zhipu.ConfigFields[0].Options))
	}
	if zhipu.ConfigFields[1].Key != "content_size" || zhipu.ConfigFields[1].Default != "medium" {
		t.Fatalf("unexpected content size config metadata: %+v", zhipu.ConfigFields[1])
	}
}

// TestVolcengineProviderMetadataExposesNoEditableRequestOptions pins the
// deliberate absence of ConfigFields. The same vendor is also driven by a
// version-controlled search policy outside this service; a request option that
// could be edited per tenant in the settings UI would be a policy the project
// repository no longer describes, so the credential and the proxy are all this
// provider accepts.
func TestVolcengineProviderMetadataExposesNoEditableRequestOptions(t *testing.T) {
	var volcengine *WebSearchProviderTypeInfo
	providerTypes := GetWebSearchProviderTypes()
	for i := range providerTypes {
		if providerTypes[i].ID == string(WebSearchProviderTypeVolcengine) {
			volcengine = &providerTypes[i]
			break
		}
	}
	if volcengine == nil {
		t.Fatal("Volcengine provider metadata is missing")
	}
	if !volcengine.RequiresAPIKey || !volcengine.SupportsProxy {
		t.Fatalf("unexpected Volcengine capability metadata: %+v", volcengine)
	}
	if volcengine.RequiresEngineID || volcengine.RequiresBaseURL || volcengine.SupportsOptionalAPIKey {
		t.Fatalf("Volcengine should need only an API key: %+v", volcengine)
	}
	if len(volcengine.ConfigFields) != 0 {
		t.Fatalf("len(ConfigFields) = %d, want 0", len(volcengine.ConfigFields))
	}
}
