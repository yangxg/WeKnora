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

func TestGetWebSearchProviderTypesIncludesExa(t *testing.T) {
	var exa *WebSearchProviderTypeInfo
	providerTypes := GetWebSearchProviderTypes()
	for i := range providerTypes {
		if providerTypes[i].ID == string(WebSearchProviderTypeExa) {
			exa = &providerTypes[i]
			break
		}
	}
	if exa == nil {
		t.Fatal("Exa provider metadata is missing")
	}
	if !exa.RequiresAPIKey || !exa.SupportsProxy {
		t.Fatalf("unexpected Exa capability metadata: %+v", exa)
	}
	if len(exa.ConfigFields) != 1 {
		t.Fatalf("len(ConfigFields) = %d, want 1", len(exa.ConfigFields))
	}
	field := exa.ConfigFields[0]
	if field.Key != "include_text" || field.Type != "select" || field.Default != "false" {
		t.Fatalf("unexpected Exa config metadata: %+v", field)
	}
	if len(field.Options) != 2 || field.Options[0].Value != "true" || field.Options[1].Value != "false" {
		t.Fatalf("unexpected Exa config options: %+v", field.Options)
	}
}
func TestGetWebSearchProviderTypesIncludesMetaso(t *testing.T) {
	var metaso *WebSearchProviderTypeInfo
	providerTypes := GetWebSearchProviderTypes()
	for i := range providerTypes {
		if providerTypes[i].ID == string(WebSearchProviderTypeMetaso) {
			metaso = &providerTypes[i]
			break
		}
	}
	if metaso == nil {
		t.Fatal("Metaso provider type not found")
	}
	if !metaso.RequiresAPIKey || !metaso.SupportsProxy || len(metaso.ConfigFields) != 1 {
		t.Fatalf("unexpected Metaso metadata: %+v", metaso)
	}
	if field := metaso.ConfigFields[0]; field.Key != "scope" || field.Default != "webpage" || len(field.Options) != 6 {
		t.Fatalf("unexpected Metaso scope metadata: %+v", field)
	}
}
