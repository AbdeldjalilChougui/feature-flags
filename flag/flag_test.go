package flag

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func boolFlag(cfg EnvConfig) EnvFlag {
	return EnvFlag{Key: "new-checkout", Salt: "s4lt", Variants: map[string]any{"on": true, "off": false}, EnvConfig: cfg}
}

func TestOffAndDefault(t *testing.T) {
	f := boolFlag(EnvConfig{Enabled: false, OffVariant: "off", Default: Serve{Variant: "on"}})
	if r := Evaluate(f, Context{"key": "u1"}); r.Value != false || r.Reason.Kind != "OFF" {
		t.Fatalf("%+v", r)
	}
	f.Enabled = true
	if r := Evaluate(f, Context{"key": "u1"}); r.Value != true || r.Reason.Kind != "DEFAULT" {
		t.Fatalf("%+v", r)
	}
}

func TestRules(t *testing.T) {
	f := boolFlag(EnvConfig{Enabled: true, OffVariant: "off", Default: Serve{Variant: "off"}, Rules: []Rule{
		{Clauses: []Clause{{Attribute: "email", Op: "ends_with", Values: []any{"@example.dz"}}}, Serve: Serve{Variant: "on"}},
		{Clauses: []Clause{
			{Attribute: "country", Op: "in", Values: []any{"DZ", "TN"}},
			{Attribute: "age", Op: "gte", Values: []any{18}},
		}, Serve: Serve{Variant: "on"}},
		{Clauses: []Clause{{Attribute: "groups", Op: "eq", Values: []any{"beta"}}}, Serve: Serve{Variant: "on"}},
		{Clauses: []Clause{{Attribute: "plan", Op: "not_in", Values: []any{"free"}}}, Serve: Serve{Variant: "on"}},
	}})
	cases := []struct {
		ctx  Context
		want bool
		rule int
	}{
		{Context{"key": "1", "email": "a@example.dz"}, true, 1},
		{Context{"key": "2", "country": "DZ", "age": 30.0}, true, 2}, // float64, as from JSON
		{Context{"key": "3", "country": "DZ", "age": 16}, false, 0},  // int, as from Go
		{Context{"key": "4", "country": "FR", "age": 40}, false, 0},
		{Context{"key": "5", "groups": []any{"staff", "beta"}}, true, 3},
		{Context{"key": "6", "groups": []string{"beta"}}, true, 3},
		{Context{"key": "7", "plan": "pro"}, true, 4},
		{Context{"key": "8", "plan": "free"}, false, 0},
		{Context{"key": "9"}, false, 0}, // missing attribute: not_in does NOT match
	}
	for _, c := range cases {
		r := Evaluate(f, c.ctx)
		if r.Value != c.want || r.Reason.RuleIndex != c.rule {
			t.Errorf("%v: got %+v", c.ctx, r)
		}
	}
}

func TestStringOps(t *testing.T) {
	ctx := Context{"ua": "Mozilla/5.0 (Android 14)", "n": 5}
	for _, c := range []struct {
		cl   Clause
		want bool
	}{
		{Clause{"ua", "contains", []any{"Android"}}, true},
		{Clause{"ua", "starts_with", []any{"Mozilla"}}, true},
		{Clause{"ua", "matches", []any{`Android 1[0-9]`}}, true},
		{Clause{"ua", "matches", []any{`iPhone`}}, false},
		{Clause{"n", "contains", []any{"5"}}, false}, // type mismatch never matches
		{Clause{"n", "lt", []any{10}}, true},
		{Clause{"n", "neq", []any{5.0}}, false},
	} {
		if got := matchClause(c.cl, ctx); got != c.want {
			t.Errorf("%+v: %v", c.cl, got)
		}
	}
}

func TestRolloutDistributionAndStickiness(t *testing.T) {
	f := boolFlag(EnvConfig{Enabled: true, OffVariant: "off",
		Default: Serve{Rollout: []Weighted{{"on", 2000}, {"off", 8000}}}})
	on := 0
	const n = 20000
	first := map[string]bool{}
	for i := 0; i < n; i++ {
		key := fmt.Sprint("user-", i)
		r := Evaluate(f, Context{"key": key})
		if r.Value == true {
			on++
			first[key] = true
		}
		if !r.Reason.Rollout {
			t.Fatal("reason should flag the rollout")
		}
		// Same input, same answer.
		if Evaluate(f, Context{"key": key}).Value != r.Value {
			t.Fatal("not deterministic")
		}
	}
	if frac := float64(on) / n; math.Abs(frac-0.20) > 0.015 {
		t.Fatalf("20%% rollout gave %.3f", frac)
	}
	// Widening to 50% keeps everyone who already had it.
	f.Default.Rollout = []Weighted{{"on", 5000}, {"off", 5000}}
	for key := range first {
		if Evaluate(f, Context{"key": key}).Value != true {
			t.Fatalf("%s lost the feature when the rollout grew", key)
		}
	}
	// A different flag (or salt) buckets users independently.
	g := f
	g.Key = "other-flag"
	same := 0
	for i := 0; i < 1000; i++ {
		k := fmt.Sprint("user-", i)
		if Bucket(f.Key, f.Salt, k)/1000 == Bucket(g.Key, g.Salt, k)/1000 {
			same++
		}
	}
	if same > 200 {
		t.Fatalf("buckets correlate across flags: %d/1000 in the same decile", same)
	}
	// bucket_by another attribute; missing attribute → error, off variant.
	f.Default.BucketBy = "org"
	if r := Evaluate(f, Context{"key": "x"}); r.Reason.Kind != "ERROR" || r.Value != false {
		t.Fatalf("%+v", r)
	}
	a := Evaluate(f, Context{"key": "x", "org": "acme"})
	b := Evaluate(f, Context{"key": "y", "org": "acme"})
	if a.Value != b.Value {
		t.Fatal("same org must get the same variant")
	}
}

func TestValidate(t *testing.T) {
	good := &Flag{Key: "dark-mode", Variants: map[string]any{"on": true, "off": false}, Salt: "x",
		Environments: map[string]*EnvConfig{"prod": {OffVariant: "off", Default: Serve{Variant: "on"}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := &Flag{Key: "Bad Key", Variants: map[string]any{"a": 1}, Environments: map[string]*EnvConfig{"prod": {
		OffVariant: "zz",
		Default:    Serve{Rollout: []Weighted{{"a", 5000}}},
		Rules:      []Rule{{Clauses: []Clause{{Attribute: "x", Op: "like", Values: []any{1}}}, Serve: Serve{Variant: "b"}}},
	}}}
	err := bad.Validate()
	for _, want := range []string{"lowercase", "off_variant", "add up to 10000", "unknown operator", `variant "b" does not exist`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	raw := `{"key":"banner","variants":{"a":"blue","b":"green"},"salt":"1","environments":{"prod":{"enabled":true,"off_variant":"a",
	  "rules":[{"clauses":[{"attribute":"country","op":"eq","values":["DZ"]}],"serve":{"variant":"b"}}],"default":{"variant":"a"}}}}`
	var f Flag
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	ef, _ := f.ForEnv("prod")
	if r := Evaluate(ef, Context{"key": "1", "country": "DZ"}); r.Value != "green" {
		t.Fatalf("%+v", r)
	}
}
