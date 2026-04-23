package shardmap

import (
	"bytes"
	"math/rand"
	"strconv"
	"testing"

	"github.com/dopecoder/cachemoney/internal/hash"
)

// ===========================================================================
// Increment 5 — property / fuzz suite: model equivalence to a reference Go map
// ===========================================================================
//
// This file drives randomized operation streams against the sharded Map and an
// equivalent reference map[K]V, asserting every Get (presence + value) and every
// Len agree. The structural Robin Hood invariants are re-checked per shard via the
// checkMapInvariants white-box hook (invariants_test.go). Together they complete
// spec "Behavior matches a reference map under random operations" at full sharded
// scope (design §9, §10, §16) and lock the increment-5 acceptance gate.

// runModelEquivalence drives ops randomized Set/Get/Delete operations (mixed by the
// supplied weights) against both a fresh *Map and a reference map[string]int, using
// a seeded PRNG so failures reproduce. It asserts presence/value on every Get,
// existed-reporting on every Delete, and that Len tracks the model after every
// operation, with the per-shard structural invariants re-checked periodically and
// at the end.
func runModelEquivalence(t *testing.T, shards, keySpace, ops, wWrite, wDelete, wRead int, seed int64) {
	t.Helper()

	m := New[string, int](hashByFNV, WithShards(shards))
	model := make(map[string]int)
	rng := rand.New(rand.NewSource(seed))
	total := wWrite + wDelete + wRead

	for n := 0; n < ops; n++ {
		key := "key" + strconv.Itoa(rng.Intn(keySpace))
		switch r := rng.Intn(total); {
		case r < wWrite:
			val := rng.Int()
			m.Set(key, val)
			model[key] = val
		case r < wWrite+wDelete:
			_, gotOK := m.Delete(key)
			_, wantOK := model[key]
			if gotOK != wantOK {
				t.Fatalf("op %d Delete(%q) existed = %v, want %v", n, key, gotOK, wantOK)
			}
			delete(model, key)
		default:
			gotV, gotOK := m.Get(key)
			wantV, wantOK := model[key]
			if gotOK != wantOK || (wantOK && gotV != wantV) {
				t.Fatalf("op %d Get(%q) = (%d, %v), want (%d, %v)", n, key, gotV, gotOK, wantV, wantOK)
			}
		}
		if got := m.Len(); got != len(model) {
			t.Fatalf("op %d Len = %d, want %d", n, got, len(model))
		}
		if n%1000 == 0 {
			checkMapInvariants(t, m)
		}
	}

	// Final reconciliation: every live key found with the right value, every absent
	// key in the space reads missing, and the structure is still sound.
	if got := m.Len(); got != len(model) {
		t.Fatalf("final Len = %d, want %d", got, len(model))
	}
	for k, want := range model {
		if v, ok := m.Get(k); !ok || v != want {
			t.Fatalf("final Get(%q) = (%d, %v), want (%d, true)", k, v, ok, want)
		}
	}
	for i := 0; i < keySpace; i++ {
		k := "key" + strconv.Itoa(i)
		if _, live := model[k]; live {
			continue
		}
		if _, ok := m.Get(k); ok {
			t.Fatalf("absent Get(%q) = true, want false", k)
		}
	}
	checkMapInvariants(t, m)
}

// TestMap_ModelEquivalence is the increment-5 model-equivalence gate. It varies
// shard count, key cardinality, and the write/delete/read mix (TRIANGULATE) so no
// single favorable configuration is cherry-picked: a degenerate single shard, a
// write-heavy small-key churn, a delete-heavy tiny-key stress, and a read-heavy
// wide-key run.
func TestMap_ModelEquivalence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                   string
		shards, keySpace, ops  int
		wWrite, wDelete, wRead int
		seed                   int64
	}{
		{"single-shard/tiny-keys/delete-heavy", 1, 16, 12000, 3, 5, 2, 0x01},
		{"few-shards/small-keys/write-heavy", 2, 64, 12000, 6, 2, 2, 0x02},
		{"mid-shards/mid-keys/read-heavy", 16, 512, 12000, 2, 1, 7, 0x03},
		{"many-shards/wide-keys/balanced", 64, 4096, 8000, 4, 3, 3, 0x04},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runModelEquivalence(t, tc.shards, tc.keySpace, tc.ops,
				tc.wWrite, tc.wDelete, tc.wRead, tc.seed)
		})
	}
}

// TestMap_ModelEquivalenceByteValues varies the value SIZE axis (TRIANGULATE): it
// stores []byte values of differing lengths against a reference map[string][]byte,
// asserting byte-for-byte equivalence under churn. This both exercises a non-scalar
// V through grow/shrink/backward-shift and pairs with the copy-on-write lock in
// invariants_test.go.
func TestMap_ModelEquivalenceByteValues(t *testing.T) {
	t.Parallel()

	m := New[string, []byte](hashByFNV, WithShards(8))
	model := make(map[string][]byte)
	rng := rand.New(rand.NewSource(0xB17E5))

	const (
		ops      = 15000
		keySpace = 256
	)
	for n := 0; n < ops; n++ {
		key := "k" + strconv.Itoa(rng.Intn(keySpace))
		switch rng.Intn(4) {
		case 0, 1: // write a fresh value of a random size (incl. empty)
			v := make([]byte, rng.Intn(64))
			for j := range v {
				v[j] = byte(rng.Intn(256))
			}
			m.Set(key, v)
			model[key] = append([]byte(nil), v...)
		case 2: // delete
			_, gotOK := m.Delete(key)
			_, wantOK := model[key]
			if gotOK != wantOK {
				t.Fatalf("op %d Delete(%q) existed = %v, want %v", n, key, gotOK, wantOK)
			}
			delete(model, key)
		default: // read
			got, gotOK := m.Get(key)
			want, wantOK := model[key]
			if gotOK != wantOK {
				t.Fatalf("op %d Get(%q) ok = %v, want %v", n, key, gotOK, wantOK)
			}
			if wantOK && !bytes.Equal(got, want) {
				t.Fatalf("op %d Get(%q) = %x, want %x", n, key, got, want)
			}
		}
		if got := m.Len(); got != len(model) {
			t.Fatalf("op %d Len = %d, want %d", n, got, len(model))
		}
	}
	for k, want := range model {
		got, ok := m.Get(k)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("final Get(%q) = (%x, %v), want (%x, true)", k, got, ok, want)
		}
	}
	checkMapInvariants(t, m)
}

// TestMap_DeterministicUnderFixedSeed pins that the same injected WithSeed yields
// identical routing and therefore identical observable behavior across two
// independently constructed maps driven by the same operation stream — the
// determinism the property/fuzz suite relies on (design §5; spec "Seeded hashing is
// per-process random yet deterministic under a fixed seed").
func TestMap_DeterministicUnderFixedSeed(t *testing.T) {
	t.Parallel()

	seed := hash.NewSeed()
	a := New[string, int](hash.String, WithShards(32), WithSeed(seed))
	b := New[string, int](hash.String, WithShards(32), WithSeed(seed))
	rng := rand.New(rand.NewSource(0x5EED1E))

	const (
		ops      = 20000
		keySpace = 512
	)
	for n := 0; n < ops; n++ {
		key := "k" + strconv.Itoa(rng.Intn(keySpace))
		switch rng.Intn(4) {
		case 0, 1:
			v := rng.Int()
			a.Set(key, v)
			b.Set(key, v)
		case 2:
			av, aok := a.Get(key)
			bv, bok := b.Get(key)
			if aok != bok || av != bv {
				t.Fatalf("op %d Get(%q): a = (%d, %v), b = (%d, %v)", n, key, av, aok, bv, bok)
			}
		default:
			_, aok := a.Delete(key)
			_, bok := b.Delete(key)
			if aok != bok {
				t.Fatalf("op %d Delete(%q): a existed = %v, b existed = %v", n, key, aok, bok)
			}
		}
		if a.Len() != b.Len() {
			t.Fatalf("op %d Len: a = %d, b = %d", n, a.Len(), b.Len())
		}
	}

	// Routing determinism: a fixed seed maps each key to the same shard on both maps.
	for i := 0; i < keySpace; i++ {
		k := "k" + strconv.Itoa(i)
		ai := shardIndex(hash.String(seed, k), a.shardShift)
		bi := shardIndex(hash.String(seed, k), b.shardShift)
		if ai != bi {
			t.Fatalf("routing for %q differs: a shard %d, b shard %d", k, ai, bi)
		}
	}
}

// FuzzMapModelEquivalence fuzzes operation streams: each 3-byte group decodes to
// (opcode, key, value), driving both the Map and a reference map[string]int and
// asserting equivalence plus the structural invariants after the stream. The
// committed seed corpus (f.Add below) keeps a bounded `-fuzz ... -fuzztime` smoke
// reproducible in CI; longer local runs explore beyond it.
func FuzzMapModelEquivalence(f *testing.F) {
	f.Add([]byte{0x00, 0x01, 0x05, 0x01, 0x01, 0x00, 0x02, 0x01, 0x00})
	f.Add([]byte("the quick brown fox jumps over 13 lazy dogs!"))
	f.Add(bytes.Repeat([]byte{0xAB, 0x07, 0xFE}, 64))
	f.Add(bytes.Repeat([]byte{0x00, 0x00, 0x00}, 90)) // all-collide on key "k0"
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		m := New[string, int](hashByFNV, WithShards(8))
		model := make(map[string]int)

		for i := 0; i+2 < len(data); i += 3 {
			key := "k" + strconv.Itoa(int(data[i+1]))
			switch data[i] % 3 {
			case 0:
				val := int(data[i+2])
				m.Set(key, val)
				model[key] = val
			case 1:
				gotV, gotOK := m.Get(key)
				wantV, wantOK := model[key]
				if gotOK != wantOK || (wantOK && gotV != wantV) {
					t.Fatalf("Get(%q) = (%d, %v), want (%d, %v)", key, gotV, gotOK, wantV, wantOK)
				}
			default:
				_, gotOK := m.Delete(key)
				_, wantOK := model[key]
				if gotOK != wantOK {
					t.Fatalf("Delete(%q) existed = %v, want %v", key, gotOK, wantOK)
				}
				delete(model, key)
			}
			if got := m.Len(); got != len(model) {
				t.Fatalf("Len = %d, want %d", got, len(model))
			}
		}

		for k, want := range model {
			if v, ok := m.Get(k); !ok || v != want {
				t.Fatalf("final Get(%q) = (%d, %v), want (%d, true)", k, v, ok, want)
			}
		}
		checkMapInvariants(t, m)
	})
}
