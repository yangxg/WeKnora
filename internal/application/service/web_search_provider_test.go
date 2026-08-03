package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestValidateProviderParametersZhipu(t *testing.T) {
	valid := types.WebSearchProviderParameters{
		APIKey: "key",
		ExtraConfig: map[string]string{
			"search_engine": "search_pro",
			"content_size":  "high",
		},
	}
	if err := validateProviderParameters(types.WebSearchProviderTypeZhipu, valid); err != nil {
		t.Fatalf("valid Zhipu parameters rejected: %v", err)
	}

	invalid := valid
	invalid.ExtraConfig = map[string]string{"search_engine": "unsupported"}
	if err := validateProviderParameters(types.WebSearchProviderTypeZhipu, invalid); err == nil {
		t.Fatal("invalid Zhipu search engine was accepted")
	}
}

func TestIsValidProviderTypeIncludesZhipu(t *testing.T) {
	if !isValidProviderType(types.WebSearchProviderTypeZhipu) {
		t.Fatal("Zhipu provider type is not accepted")
	}
}

func TestValidateProviderParametersVolcengine(t *testing.T) {
	if !isValidProviderType(types.WebSearchProviderTypeVolcengine) {
		t.Fatal("Volcengine provider type is not accepted")
	}

	valid := types.WebSearchProviderParameters{APIKey: "key"}
	if err := validateProviderParameters(types.WebSearchProviderTypeVolcengine, valid); err != nil {
		t.Fatalf("valid Volcengine parameters rejected: %v", err)
	}

	if err := validateProviderParameters(
		types.WebSearchProviderTypeVolcengine, types.WebSearchProviderParameters{},
	); err == nil {
		t.Fatal("Volcengine parameters without an API key were accepted")
	}
}
