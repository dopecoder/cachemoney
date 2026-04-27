package cache

import (
	"strconv"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/hash"
)

// TestOptions_ShardAndSeedReachShardmap is a white-box check that WithShards and
// WithSeed are collected as pass-through shardmap options and actually reach the
// constructed *shardmap.Map. The Cache method bodies are still stubs in this
// increment, so the backing map is exercised directly through c.m.
func TestOptions_ShardAndSeedReachShardmap(t *testing.T) {
	// Plumbing: each pass-through option appends exactly one shardmap.Option.
	var cfg config
	WithShards(8)(&cfg)
	WithSeed(hash.NewSeed())(&cfg)
	if got := len(cfg.shardOpts); got != 2 {
		t.Fatalf("WithShards+WithSeed appended %d shardmap options, want 2", got)
	}

	// Functional: New must build a usable multi-shard map from those options. If
	// the options were dropped (or never passed to shardmap.New) the keyspace
	// would not round-trip across shards.
	c := New(WithShards(8), WithSeed(hash.NewSeed()))
	if c.m == nil {
		t.Fatal("New did not construct the backing shardmap")
	}
	const n = 256
	for i := 0; i < n; i++ {
		c.m.Set(strconv.Itoa(i), entry{value: []byte{byte(i)}})
	}
	if got := c.m.Len(); got != n {
		t.Fatalf("backing shardmap Len = %d, want %d (pass-through options did not build a working map)", got, n)
	}
}

// TestEntry_RoundTripsThroughShardmap confirms the engine's chosen value type —
// entry{value, expiresAt} (design §4.1 option-b) — stores and reads back through
// the backing shardmap with both fields intact, even before the TTL/copy bodies
// exist. This locks the entry shape the increment-7 Get/Set will build on.
func TestEntry_RoundTripsThroughShardmap(t *testing.T) {
	c := New()
	exp := time.Unix(1_700_000_100, 0)
	c.m.Set("k", entry{value: []byte("v"), expiresAt: exp})

	got, ok := c.m.Get("k")
	if !ok {
		t.Fatal("stored entry not found in backing shardmap")
	}
	if string(got.value) != "v" {
		t.Errorf("entry.value = %q, want %q", got.value, "v")
	}
	if !got.expiresAt.Equal(exp) {
		t.Errorf("entry.expiresAt = %v, want %v", got.expiresAt, exp)
	}
}

// TestWithClock_DefaultsToTimeNow confirms the clock seam is wired: New defaults
// the clock to time.Now and WithClock overrides it. now is read here (white-box)
// because the read-op bodies that consume it land in increments 7–8.
func TestWithClock_DefaultsToTimeNow(t *testing.T) {
	if c := New(); c.now == nil {
		t.Fatal("New() left the clock nil; want default time.Now")
	}

	fixed := time.Unix(1_700_000_000, 0)
	c := New(WithClock(func() time.Time { return fixed }))
	if got := c.now(); !got.Equal(fixed) {
		t.Fatalf("WithClock clock = %v, want %v", got, fixed)
	}

	// A nil clock is ignored, leaving the default in place.
	if c := New(WithClock(nil)); c.now == nil {
		t.Fatal("WithClock(nil) cleared the clock; want default retained")
	}
}
