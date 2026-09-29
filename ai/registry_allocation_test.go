package ai

import (
	"reflect"
	"testing"
)

// Filtering keeps the generated provider and model order and returns an owned slice.
func TestListModelsAllocatesOnlyFilteredCatalog(t *testing.T) {
	providers := append(ListProviders(), "", "missing-provider")
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			want := []GeneratedModel{}
			for _, model := range GeneratedModels {
				if provider == "" || model.Provider == provider {
					want = append(want, model)
				}
			}
			got := ListModels(provider)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("filtered catalog differs from generated order or metadata")
			}
			if cap(got) != len(got) {
				t.Errorf("filtered catalog capacity = %d, want result length %d", cap(got), len(got))
			}
			if len(got) > 0 {
				got[0].ID = "modified copy"
				if ListModels(provider)[0].ID != want[0].ID {
					t.Fatal("caller mutated catalog")
				}
			}
		})
	}
}

func BenchmarkListModelsByProvider(b *testing.B) {
	providers := ListProviders()
	b.ReportAllocs()
	for b.Loop() {
		for _, provider := range providers {
			_ = ListModels(provider)
		}
	}
}
