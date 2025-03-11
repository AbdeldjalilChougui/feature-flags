// Package flag defines feature flags and evaluates them. The server and the
// SDK share this code, so a flag evaluates the same everywhere.
//
// A flag has named variants (any JSON values, e.g. on=true/off=false or
// "blue"/"green"). Each environment (production, staging, …) has its own
// configuration:
//
//	enabled      false → everyone gets off_variant
//	rules        evaluated in order; the first rule whose clauses all match
//	             decides (serve one variant, or a percentage rollout)
//	default      what everyone else gets (variant or rollout)
//
// Percentage rollouts use consistent hashing: a user's bucket is derived
// from sha256(flag key, flag salt, user key), so the same user always gets
// the same variant, and widening a rollout from 10% to 20% keeps the
// original 10%.
package flag

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
)

// Context describes who/what is being evaluated. "key" identifies the
// user (or device, or organisation) and is used for rollouts by default.
type Context map[string]any

// Flag is the full definition, across environments.
type Flag struct {
	Key          string                `json:"key"`
	Description  string                `json:"description,omitempty"`
	Variants     map[string]any        `json:"variants"`
	Salt         string                `json:"salt"`
	Environments map[string]*EnvConfig `json:"environments"`
}

// EnvConfig is a flag's configuration in one environment.
type EnvConfig struct {
	Enabled    bool   `json:"enabled"`
	OffVariant string `json:"off_variant"`
	Rules      []Rule `json:"rules,omitempty"`
	Default    Serve  `json:"default"`
}

// Rule matches when all its clauses match.
type Rule struct {
	Description string   `json:"description,omitempty"`
	Clauses     []Clause `json:"clauses"`
	Serve       Serve    `json:"serve"`
}

// Clause tests one context attribute.
type Clause struct {
	Attribute string `json:"attribute"`
	Op        string `json:"op"`
	Values    []any  `json:"values"`
}

// Serve picks a fixed variant or splits by weight (basis points, total 10000).
type Serve struct {
	Variant  string     `json:"variant,omitempty"`
	Rollout  []Weighted `json:"rollout,omitempty"`
	BucketBy string     `json:"bucket_by,omitempty"` // context attribute, default "key"
}

type Weighted struct {
	Variant string `json:"variant"`
	Weight  int    `json:"weight"` // basis points: 2500 = 25%
}

// Ops lists the supported clause operators.
var Ops = []string{"eq", "neq", "in", "not_in", "contains", "starts_with", "ends_with", "matches", "gt", "gte", "lt", "lte"}

var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)

// EnvFlag is a flag resolved for one environment: everything needed to
// evaluate it, and what the SDK downloads.
type EnvFlag struct {
	Key      string         `json:"key"`
	Variants map[string]any `json:"variants"`
	Salt     string         `json:"salt"`
	EnvConfig
}

// ForEnv extracts the environment's view. ok is false if the flag is not
// configured there.
func (f *Flag) ForEnv(env string) (EnvFlag, bool) {
	c, ok := f.Environments[env]
	if !ok || c == nil {
		return EnvFlag{}, false
	}
	return EnvFlag{Key: f.Key, Variants: f.Variants, Salt: f.Salt, EnvConfig: *c}, true
}

// Validate checks the flag's internal consistency.
func (f *Flag) Validate() error {
	var errs []error
	if !keyRe.MatchString(f.Key) {
		errs = append(errs, fmt.Errorf("key %q must be lowercase letters, digits, '.', '_' or '-'", f.Key))
	}
	if len(f.Variants) < 1 {
		errs = append(errs, errors.New("a flag needs at least one variant"))
	}
	for env, c := range f.Environments {
		if c == nil {
			continue
		}
		if err := c.Validate(f.Variants); err != nil {
			errs = append(errs, fmt.Errorf("environment %s: %w", env, err))
		}
	}
	return errors.Join(errs...)
}

// Validate checks an environment config against the flag's variants.
func (c *EnvConfig) Validate(variants map[string]any) error {
	var errs []error
	has := func(v string) bool { _, ok := variants[v]; return ok }
	if !has(c.OffVariant) {
		errs = append(errs, fmt.Errorf("off_variant %q is not a variant", c.OffVariant))
	}
	checkServe := func(where string, s Serve) {
		switch {
		case s.Variant != "" && len(s.Rollout) > 0:
			errs = append(errs, fmt.Errorf("%s: use either variant or rollout, not both", where))
		case s.Variant != "":
			if !has(s.Variant) {
				errs = append(errs, fmt.Errorf("%s: variant %q does not exist", where, s.Variant))
			}
		case len(s.Rollout) > 0:
			total := 0
			for _, w := range s.Rollout {
				if !has(w.Variant) {
					errs = append(errs, fmt.Errorf("%s: rollout variant %q does not exist", where, w.Variant))
				}
				if w.Weight < 0 {
					errs = append(errs, fmt.Errorf("%s: negative weight", where))
				}
				total += w.Weight
			}
			if total != 10000 {
				errs = append(errs, fmt.Errorf("%s: rollout weights must add up to 10000 (100%%), got %d", where, total))
			}
		default:
			errs = append(errs, fmt.Errorf("%s: serve needs a variant or a rollout", where))
		}
	}
	checkServe("default", c.Default)
	for i, r := range c.Rules {
		where := fmt.Sprintf("rule %d", i+1)
		if len(r.Clauses) == 0 {
			errs = append(errs, fmt.Errorf("%s: needs at least one clause", where))
		}
		for _, cl := range r.Clauses {
			if cl.Attribute == "" {
				errs = append(errs, fmt.Errorf("%s: clause without attribute", where))
			}
			if !validOp(cl.Op) {
				errs = append(errs, fmt.Errorf("%s: unknown operator %q (want one of %s)", where, cl.Op, strings.Join(Ops, ", ")))
			}
			if len(cl.Values) == 0 {
				errs = append(errs, fmt.Errorf("%s: clause on %q has no values", where, cl.Attribute))
			}
			if cl.Op == "matches" {
				for _, v := range cl.Values {
					s, _ := v.(string)
					if _, err := regexp.Compile(s); err != nil {
						errs = append(errs, fmt.Errorf("%s: bad regexp %q", where, s))
					}
				}
			}
		}
		checkServe(where, r.Serve)
	}
	return errors.Join(errs...)
}

func validOp(op string) bool {
	for _, o := range Ops {
		if o == op {
			return true
		}
	}
	return false
}

// Reason explains an evaluation result.
type Reason struct {
	Kind      string `json:"kind"`                 // OFF, RULE_MATCH, DEFAULT, ERROR
	RuleIndex int    `json:"rule_index,omitempty"` // 1-based, for RULE_MATCH
	Rollout   bool   `json:"rollout,omitempty"`    // a percentage rollout chose the variant
	Error     string `json:"error,omitempty"`
}

// Result of an evaluation.
type Result struct {
	Value   any    `json:"value"`
	Variant string `json:"variant"`
	Reason  Reason `json:"reason"`
}

// Evaluate decides the variant for ctx.
func Evaluate(f EnvFlag, ctx Context) Result {
	if !f.Enabled {
		return f.result(f.OffVariant, Reason{Kind: "OFF"})
	}
	for i, r := range f.Rules {
		if matchAll(r.Clauses, ctx) {
			v, rollout, err := f.pick(r.Serve, ctx)
			if err != nil {
				return f.result(f.OffVariant, Reason{Kind: "ERROR", Error: err.Error()})
			}
			return f.result(v, Reason{Kind: "RULE_MATCH", RuleIndex: i + 1, Rollout: rollout})
		}
	}
	v, rollout, err := f.pick(f.Default, ctx)
	if err != nil {
		return f.result(f.OffVariant, Reason{Kind: "ERROR", Error: err.Error()})
	}
	return f.result(v, Reason{Kind: "DEFAULT", Rollout: rollout})
}

func (f EnvFlag) result(variant string, r Reason) Result {
	return Result{Value: f.Variants[variant], Variant: variant, Reason: r}
}

func (f EnvFlag) pick(s Serve, ctx Context) (string, bool, error) {
	if len(s.Rollout) == 0 {
		return s.Variant, false, nil
	}
	by := s.BucketBy
	if by == "" {
		by = "key"
	}
	id, ok := ctx[by]
	if !ok || id == nil || fmt.Sprint(id) == "" {
		return "", true, fmt.Errorf("rollout needs context attribute %q", by)
	}
	b := Bucket(f.Key, f.Salt, fmt.Sprint(id))
	acc := 0
	for _, w := range s.Rollout {
		acc += w.Weight
		if b < acc {
			return w.Variant, true, nil
		}
	}
	return s.Rollout[len(s.Rollout)-1].Variant, true, nil
}

// Bucket maps (flag, salt, id) to 0..9999, uniformly and deterministically.
func Bucket(flagKey, salt, id string) int {
	h := sha256.Sum256([]byte(flagKey + "." + salt + "." + id))
	x := binary.BigEndian.Uint64(h[:8])
	return int(x % 10000)
}

func matchAll(cs []Clause, ctx Context) bool {
	for _, c := range cs {
		if !matchClause(c, ctx) {
			return false
		}
	}
	return true
}

// matchClause: a missing attribute never matches (not even neq/not_in), so
// rules don't accidentally target users we know nothing about. A list
// attribute (e.g. groups) matches if any element matches.
func matchClause(c Clause, ctx Context) bool {
	v, ok := ctx[c.Attribute]
	if !ok || v == nil {
		return false
	}
	negate := c.Op == "neq" || c.Op == "not_in"
	if ss, ok := v.([]string); ok {
		l := make([]any, len(ss))
		for i, x := range ss {
			l[i] = x
		}
		v = l
	}
	if list, isList := v.([]any); isList {
		found := false
		for _, e := range list {
			if matchValue(c, e) {
				found = true
				break
			}
		}
		if negate {
			return !found
		}
		return found
	}
	m := matchValue(c, v)
	if negate {
		return !m
	}
	return m
}

// matchValue applies the positive form of the operator to one value.
func matchValue(c Clause, v any) bool {
	for _, want := range c.Values {
		switch c.Op {
		case "eq", "neq", "in", "not_in":
			if equal(v, want) {
				return true
			}
		case "contains", "starts_with", "ends_with", "matches":
			s, ok1 := v.(string)
			w, ok2 := want.(string)
			if !ok1 || !ok2 {
				continue
			}
			switch c.Op {
			case "contains":
				if strings.Contains(s, w) {
					return true
				}
			case "starts_with":
				if strings.HasPrefix(s, w) {
					return true
				}
			case "ends_with":
				if strings.HasSuffix(s, w) {
					return true
				}
			case "matches":
				if re, err := regexp.Compile(w); err == nil && re.MatchString(s) {
					return true
				}
			}
		case "gt", "gte", "lt", "lte":
			a, ok1 := number(v)
			b, ok2 := number(want)
			if !ok1 || !ok2 {
				continue
			}
			if (c.Op == "gt" && a > b) || (c.Op == "gte" && a >= b) || (c.Op == "lt" && a < b) || (c.Op == "lte" && a <= b) {
				return true
			}
		}
	}
	return false
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return math.NaN(), false
}

// equal compares JSON-ish values; numbers compare numerically whatever
// their Go type (int from Go code, float64 from JSON).
func equal(a, b any) bool {
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}
