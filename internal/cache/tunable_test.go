package cache

import (
	"errors"
	"strconv"
	"testing"
)

// These tests complete the I4 contract coverage: the noeviction ErrOOM branch and
// its shrinking-overwrite exception (engine side of spec Req 7), the oversized-
// newcomer termination of the eviction loop (C-INV-1 "within one entry's cost"), the
// Tunable live-setter surface, the policy name parsing/printing (the single source of
// truth the server and main share), and the sketch-sizing clamps.

func TestParsePolicyAndString(t *testing.T) {
	t.Parallel()

	valid := map[string]EvictionPolicy{
		"allkeys-lfu":    PolicyAllKeysLFU,
		"allkeys-random": PolicyAllKeysRandom,
		"noeviction":     PolicyNoEviction,
	}
	for name, want := range valid {
		got, ok := ParsePolicy(name)
		if !ok || got != want {
			t.Fatalf("ParsePolicy(%q) = (%v, %v), want (%v, true)", name, got, ok, want)
		}
		if got.String() != name {
			t.Fatalf("%v.String() = %q, want %q", want, got.String(), name)
		}
	}
	for _, bad := range []string{"volatile-lru", "volatile-ttl", "", "garbage"} {
		if _, ok := ParsePolicy(bad); ok {
			t.Fatalf("ParsePolicy(%q) accepted an unsupported policy", bad)
		}
	}
	if s := EvictionPolicy(99).String(); s != "unknown" {
		t.Fatalf("unknown policy String() = %q, want \"unknown\"", s)
	}
}

func TestTunableLiveSetters(t *testing.T) {
	t.Parallel()

	c := New(WithShards(2))
	defer func() { _ = c.Close() }()

	var tn Tunable = c // *Cache must satisfy the optional Tunable interface
	tn.SetMaxMemory(4096)
	if got := tn.MaxMemory(); got != 4096 {
		t.Fatalf("MaxMemory() = %d, want 4096", got)
	}
	tn.SetEvictionPolicy(PolicyAllKeysRandom)
	if got := tn.EvictionPolicy(); got != PolicyAllKeysRandom {
		t.Fatalf("EvictionPolicy() = %v, want allkeys-random", got)
	}
	tn.SetMaxMemory(0) // back to unbounded
	if got := tn.MaxMemory(); got != 0 {
		t.Fatalf("MaxMemory() = %d, want 0", got)
	}
}

func TestNoEvictionRejectsAndAllowsShrink(t *testing.T) {
	t.Parallel()

	c := New(WithMaxMemory(8*1024), WithEvictionPolicy(PolicyNoEviction), WithShards(2))
	defer func() { _ = c.Close() }()

	var rejected string
	for i := 0; i < 10000; i++ {
		k := "k" + strconv.Itoa(i)
		err := c.Set(bg(), k, val(100), 0)
		if err != nil {
			if !errors.Is(err, ErrOOM) {
				t.Fatalf("unexpected error for %q: %v", k, err)
			}
			rejected = k
			break
		}
	}
	if rejected == "" {
		t.Fatal("never hit ErrOOM under noeviction at capacity")
	}
	if _, ok := mustGet(t, c, rejected); ok {
		t.Fatalf("rejected key %q was stored despite -OOM", rejected)
	}
	// A shrinking overwrite of an existing key at capacity is always allowed.
	if err := c.Set(bg(), "k0", val(1), 0); err != nil {
		t.Fatalf("shrinking overwrite at capacity rejected: %v", err)
	}
	if v, ok := mustGet(t, c, "k0"); !ok || len(v) != 1 {
		t.Fatalf("shrinking overwrite not applied: ok=%v len=%d", ok, len(v))
	}
}

func TestOversizedNewcomerStoredAndTerminates(t *testing.T) {
	t.Parallel()

	c := New(WithMaxMemory(1024), WithEvictionPolicy(PolicyAllKeysLFU), WithShards(2))
	defer func() { _ = c.Close() }()

	// A single entry larger than the whole ceiling: it must still be stored (C-INV-1)
	// and the eviction loop must terminate rather than spin (it is its own only entry,
	// and the newcomer is excluded from victim selection).
	if err := c.Set(bg(), "big", val(4096), 0); err != nil {
		t.Fatalf("oversized Set: %v", err)
	}
	v, ok := mustGet(t, c, "big")
	if !ok || len(v) != 4096 {
		t.Fatal("oversized newcomer was not stored")
	}
	if c.usage.Load() <= 1024 {
		t.Fatalf("usage %d should exceed the ceiling by the one oversized entry", c.usage.Load())
	}
}

func TestPolicyConfigClampsSketchSizing(t *testing.T) {
	t.Parallel()

	huge := New(WithMaxMemory(1<<40), WithEvictionPolicy(PolicyAllKeysLFU))
	defer func() { _ = huge.Close() }()
	if got := huge.policyConfig().Counters; got != policyCountersMax {
		t.Fatalf("huge maxmemory counters = %d, want clamp %d", got, policyCountersMax)
	}

	tiny := New(WithMaxMemory(1000), WithEvictionPolicy(PolicyAllKeysLFU))
	defer func() { _ = tiny.Close() }()
	if got := tiny.policyConfig().Counters; got != policyCountersMin {
		t.Fatalf("tiny maxmemory counters = %d, want clamp %d", got, policyCountersMin)
	}
}
