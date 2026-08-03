package interfaces

import (
	"context"
	"reflect"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

// ResearchFlow's discovery connector needs a request shape this interface
// cannot express: NeedContent, NeedUrl, Sites, BlockHosts, AuthInfoLevel,
// TimeRange, QueryRewrite, ContentFormats, Industry. ADR-0011 resolves that by
// having the connector call a shared vendor client directly, leaving this
// interface alone — chosen over widening it (nine implementations, two call
// sites, all recompiled for one consumer) and over smuggling options through
// the context (invisible to the compiler, and a silently missing profile means
// the vendor is asked for page bodies).
//
// That decision is only worth anything if the interface actually stays put, so
// its method set is pinned here. A widened Search, an added method, or an
// options-carrying variant will fail this test — which is the signal to revisit
// ADR-0011 rather than to update the assertion.
//
// Test-only; no production symbol is touched.

func TestWebSearchProviderContract_MethodSetIsFrozen(t *testing.T) {
	providerType := reflect.TypeOf((*WebSearchProvider)(nil)).Elem()

	assert.Equal(t, 2, providerType.NumMethod(),
		"a new method on WebSearchProvider means discovery options found a back door")

	name, found := providerType.MethodByName("Name")
	assert.True(t, found)
	assert.Equal(t, 0, name.Type.NumIn())
	assert.Equal(t, []reflect.Type{reflect.TypeOf("")}, outTypes(name.Type))

	search, found := providerType.MethodByName("Search")
	assert.True(t, found)
	assert.False(t, search.Type.IsVariadic(),
		"a variadic option parameter would let the discovery profile in without a compile error anywhere")
	assert.Equal(t, []reflect.Type{
		reflect.TypeOf((*context.Context)(nil)).Elem(),
		reflect.TypeOf(""),
		reflect.TypeOf(0),
		reflect.TypeOf(false),
	}, inTypes(search.Type))
	assert.Equal(t, []reflect.Type{
		reflect.TypeOf([]*types.WebSearchResult(nil)),
		reflect.TypeOf((*error)(nil)).Elem(),
	}, outTypes(search.Type))
}

func inTypes(fn reflect.Type) []reflect.Type {
	params := make([]reflect.Type, 0, fn.NumIn())
	for i := 0; i < fn.NumIn(); i++ {
		params = append(params, fn.In(i))
	}
	return params
}

func outTypes(fn reflect.Type) []reflect.Type {
	results := make([]reflect.Type, 0, fn.NumOut())
	for i := 0; i < fn.NumOut(); i++ {
		results = append(results, fn.Out(i))
	}
	return results
}
