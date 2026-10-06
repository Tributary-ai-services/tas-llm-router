package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/tributary-ai/llm-router-waf/internal/types"

	"github.com/tributary-ai/llm-router-waf/pkg/clear"
)

// pkg/clear/cost.go says it plainly: "the config and this table can drift
// independently because the config doesn't feed the scorer". They did drift, and
// the cost of the drift is silence — LookupPricing returns ok=false and the model
// simply produces no CLEAR Cost and no dollar figures, which looks like missing
// traffic rather than a missing table row. This test is the coupling that comment
// asks for.
func TestCatalogModelsArePriced(t *testing.T) {
	for vendor, models := range catalogModels(t) {
		for _, m := range models {
			for _, id := range []string{m.Name, m.ProviderModelID} {
				if id == "" {
					continue
				}
				in, out, ok := clear.LookupPricing(vendor, id)
				if !ok {
					t.Errorf("%s:%s is in the catalog but has no pricing entry in pkg/clear/cost.go — it would score and report as unpriced", vendor, id)
					continue
				}
				if in != m.InputCostPer1K || out != m.OutputCostPer1K {
					t.Errorf("%s:%s rates disagree: catalog %.5f/%.5f, cost.go %.5f/%.5f",
						vendor, id, m.InputCostPer1K, m.OutputCostPer1K, in, out)
				}
			}
		}
	}
}

// TestDefaultCatalogMatchesYAML keeps the compiled-in defaults from drifting away
// from the shipped file: the image runs configs/config.yaml, but a unit test or a
// misconfigured pod runs the defaults, and a model present in one and absent from
// the other is the same unresolved-pin failure in a place nobody looks.
func TestDefaultCatalogMatchesYAML(t *testing.T) {
	// Covers BOTH vendors. It used to check anthropic only, and that gap is
	// exactly how the OpenAI rates drifted: TestCatalogModelsArePriced reads
	// configs/config.yaml, OpenAI was commented out there, so the Go defaults'
	// OpenAI block was checked by nothing and gpt-4o sat at 0.005/0.015 against
	// 0.00250/0.01000 in pkg/clear/cost.go. The live pods run the defaults, so
	// cost-optimized routing was ranking OpenAI models on prices that CLEAR
	// disagreed with. Found and fixed 2026-10-05.
	cat := catalogModels(t)
	def := &Config{}
	def.setDefaults()

	for _, v := range []struct {
		vendor string
		models []types.ModelInfo
	}{
		{"openai", def.Providers.OpenAI.Models},
		{"anthropic", def.Providers.Anthropic.Models},
	} {
		yamlByName := map[string]catalogModel{}
		for _, m := range cat[v.vendor] {
			yamlByName[m.Name] = m
		}
		if len(yamlByName) == 0 {
			t.Errorf("no %s models in configs/config.yaml — this check would pass vacuously", v.vendor)
			continue
		}
		for _, m := range v.models {
			y, ok := yamlByName[m.Name]
			if !ok {
				t.Errorf("%s model %q is in the Go defaults but not in configs/config.yaml", v.vendor, m.Name)
				continue
			}
			// Presence is not enough: the two were present-and-disagreeing before.
			if y.InputCostPer1K != m.InputCostPer1K || y.OutputCostPer1K != m.OutputCostPer1K {
				t.Errorf("%s:%s rates disagree: config.yaml %.5f/%.5f, Go defaults %.5f/%.5f",
					v.vendor, m.Name, y.InputCostPer1K, y.OutputCostPer1K, m.InputCostPer1K, m.OutputCostPer1K)
			}
		}
	}
}

type catalogModel struct {
	Name             string  `yaml:"name"`
	ProviderModelID  string  `yaml:"provider_model_id"`
	InputCostPer1K   float64 `yaml:"input_cost_per_1k"`
	OutputCostPer1K  float64 `yaml:"output_cost_per_1k"`
	MaxContextWindow int     `yaml:"max_context_window"`
}

// catalogModels reads the shipped config directly rather than through Load(),
// which applies env overrides and would make the result depend on the
// environment the test happens to run in.
func catalogModels(t *testing.T) map[string][]catalogModel {
	t.Helper()
	path := filepath.Join("..", "..", "configs", "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Providers map[string]struct {
			Models []catalogModel `yaml:"models"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string][]catalogModel{}
	for vendor, p := range doc.Providers {
		if len(p.Models) > 0 {
			out[vendor] = p.Models
		}
	}
	if len(out) == 0 {
		t.Fatal("no provider models parsed from configs/config.yaml — the test would pass vacuously")
	}
	return out
}
