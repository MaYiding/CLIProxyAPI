package config

import "testing"

func TestParseConfigBytesBilling(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
billing:
  enabled: true
  store-path: data/billing.jsonl
  currency: CNY
  sync-on-write: true
  default-price-per-million: 1.5
  key-labels:
    client-key: customer-a
  key-limits:
    client-key: 25
  prices:
    - name: gpt-pricing
      provider: openai
      model: gpt-*
      input-per-million: 1.25
      output-per-million: 10
      reasoning-per-million: 10
      cache-read-per-million: 0.125
      cache-creation-per-million: 2
      input-cache-mode: included
      reasoning-mode: included
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if !cfg.Billing.Enabled || cfg.Billing.StorePath != "data/billing.jsonl" || cfg.Billing.Currency != "CNY" || !cfg.Billing.SyncOnWrite {
		t.Fatalf("billing config = %#v", cfg.Billing)
	}
	if cfg.Billing.KeyLabels["client-key"] != "customer-a" {
		t.Fatalf("key labels = %#v", cfg.Billing.KeyLabels)
	}
	if cfg.Billing.DefaultPricePerMillion == nil || *cfg.Billing.DefaultPricePerMillion != 1.5 {
		t.Fatalf("default price = %#v, want 1.5", cfg.Billing.DefaultPricePerMillion)
	}
	if cfg.Billing.KeyLimits["client-key"] != 25 {
		t.Fatalf("key limits = %#v", cfg.Billing.KeyLimits)
	}
	if len(cfg.Billing.Prices) != 1 {
		t.Fatalf("price rules = %d, want 1", len(cfg.Billing.Prices))
	}
	price := cfg.Billing.Prices[0]
	if price.InputPerMillion != 1.25 || price.CacheReadPerMillion != 0.125 || price.InputCacheMode != "included" {
		t.Fatalf("price = %#v", price)
	}

	clone := cfg.CloneForRuntime()
	clone.Billing.KeyLabels["client-key"] = "changed"
	*clone.Billing.DefaultPricePerMillion = 99
	clone.Billing.KeyLimits["client-key"] = 99
	clone.Billing.Prices[0].InputPerMillion = 99
	if cfg.Billing.KeyLabels["client-key"] != "customer-a" || *cfg.Billing.DefaultPricePerMillion != 1.5 || cfg.Billing.KeyLimits["client-key"] != 25 || cfg.Billing.Prices[0].InputPerMillion != 1.25 {
		t.Fatalf("CloneForRuntime() shared billing references with source")
	}
}
