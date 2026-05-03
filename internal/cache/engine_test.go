package cache_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dopecoder/cachemoney/internal/cache"
	"github.com/dopecoder/cachemoney/internal/hash"
)

// Compile-time conformance: *cache.Cache MUST implement cache.Engine. This is the
// increment-6 contract anchor — if New ever returns a type whose method set drifts
// from the frozen five-op surface, the package stops compiling here.
var _ cache.Engine = (*cache.Cache)(nil)

// TestEngine_SurfaceAndSignatures asserts the Engine interface declares exactly
// Get/Set/Del/TTL/Len with the ctx-first, error-returning signatures frozen by
// proposal §4 and the spec scenario "Engine surface and signatures are
// remote-ready". The check is done by reflection so a stray extra method, a
// dropped ctx parameter, or a reshaped result is caught structurally.
func TestEngine_SurfaceAndSignatures(t *testing.T) {
	engineType := reflect.TypeOf((*cache.Engine)(nil)).Elem()
	if engineType.Kind() != reflect.Interface {
		t.Fatalf("cache.Engine kind = %s, want interface", engineType.Kind())
	}

	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	bytesType := reflect.TypeOf([]byte(nil))
	strType := reflect.TypeOf("")
	boolType := reflect.TypeOf(true)
	intType := reflect.TypeOf(int(0))
	durType := reflect.TypeOf(time.Duration(0))

	// Each op mapped to its exact frozen signature (receiver-less, non-variadic),
	// exactly as reflect reports an interface method's Type.
	want := map[string]reflect.Type{
		"Get": reflect.FuncOf(
			[]reflect.Type{ctxType, strType},
			[]reflect.Type{bytesType, boolType, errType}, false,
		),
		"Set": reflect.FuncOf(
			[]reflect.Type{ctxType, strType, bytesType, durType},
			[]reflect.Type{errType}, false,
		),
		"Del": reflect.FuncOf(
			[]reflect.Type{ctxType, strType},
			[]reflect.Type{boolType, errType}, false,
		),
		"TTL": reflect.FuncOf(
			[]reflect.Type{ctxType, strType},
			[]reflect.Type{durType, boolType, errType}, false,
		),
		"Len": reflect.FuncOf(
			[]reflect.Type{ctxType},
			[]reflect.Type{intType, errType}, false,
		),
	}

	if got := engineType.NumMethod(); got != len(want) {
		t.Errorf("Engine declares %d methods, want exactly %d (%v)", got, len(want), methodNames(engineType))
	}

	for name, wantSig := range want {
		m, ok := engineType.MethodByName(name)
		if !ok {
			t.Errorf("Engine is missing required method %q", name)
			continue
		}
		if m.Type != wantSig {
			t.Errorf("Engine.%s signature = %s, want %s", name, m.Type, wantSig)
		}
	}

	// Independent of the exact-match table above: every op is ctx-first and
	// error-last (the remote-ready contract, ADR-0003).
	for i := 0; i < engineType.NumMethod(); i++ {
		m := engineType.Method(i)
		mt := m.Type
		if mt.NumIn() == 0 || mt.In(0) != ctxType {
			t.Errorf("Engine.%s does not take context.Context as its first parameter", m.Name)
		}
		if mt.NumOut() == 0 || mt.Out(mt.NumOut()-1) != errType {
			t.Errorf("Engine.%s does not return error as its last result", m.Name)
		}
	}
}

func methodNames(it reflect.Type) []string {
	names := make([]string, it.NumMethod())
	for i := range names {
		names[i] = it.Method(i).Name
	}
	return names
}

// TestNew_ReturnsUsableEngine is the construction smoke test: New() yields a
// non-nil *Cache that is usable as an Engine.
func TestNew_ReturnsUsableEngine(t *testing.T) {
	c := cache.New()
	if c == nil {
		t.Fatal("cache.New() = nil, want non-nil *Cache")
	}
	var _ cache.Engine = c
}

// TestNew_AcceptsOptions confirms the construction options compile and are
// accepted without panicking; their effect on the backing shardmap is checked by
// the white-box wiring test.
func TestNew_AcceptsOptions(t *testing.T) {
	c := cache.New(
		cache.WithShards(4),
		cache.WithSeed(hash.NewSeed()),
		cache.WithClock(func() time.Time { return time.Unix(0, 0) }),
	)
	if c == nil {
		t.Fatal("cache.New(opts...) = nil, want non-nil *Cache")
	}
	var _ cache.Engine = c
}

// TestOps_CancelledContextReturnsErr asserts the single ctx.Err() entry check is
// uniform across all five ops: an already-cancelled context makes every operation
// return ctx.Err() (here context.Canceled), before any lock or mutation. Together
// with TestOps_ExpiredDeadlineReturnsErr (DeadlineExceeded) this is the full
// cancellation matrix; the "a cancelled Set stores nothing" guarantee is proven
// behaviorally by TestCache_CancelledSetStoresNothing (increment 8).
func TestOps_CancelledContextReturnsErr(t *testing.T) {
	c := cache.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := c.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get cancelled err = %v, want context.Canceled", err)
	}
	if err := c.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Set cancelled err = %v, want context.Canceled", err)
	}
	if _, err := c.Del(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Del cancelled err = %v, want context.Canceled", err)
	}
	if _, _, err := c.TTL(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("TTL cancelled err = %v, want context.Canceled", err)
	}
	if _, err := c.Len(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Len cancelled err = %v, want context.Canceled", err)
	}
}

// TestOps_ExpiredDeadlineReturnsErr triangulates the cancellation entry check with
// context.DeadlineExceeded (an already-passed deadline) alongside the
// context.Canceled case above, confirming the check returns whatever ctx.Err()
// reports rather than a hard-coded error value.
func TestOps_ExpiredDeadlineReturnsErr(t *testing.T) {
	c := cache.New()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	if err := c.Set(ctx, "k", []byte("v"), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Set past-deadline err = %v, want context.DeadlineExceeded", err)
	}
	if _, _, err := c.Get(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Get past-deadline err = %v, want context.DeadlineExceeded", err)
	}
	if _, _, err := c.TTL(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("TTL past-deadline err = %v, want context.DeadlineExceeded", err)
	}
	if _, err := c.Del(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Del past-deadline err = %v, want context.DeadlineExceeded", err)
	}
	if _, err := c.Len(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Len past-deadline err = %v, want context.DeadlineExceeded", err)
	}
}

// TestOps_LiveContextReturnsNilError realizes the spec scenario "Live context
// succeeds with nil error": with a live context every op returns a nil error AND
// its effect is observable. (Increment 6 only checked the nil error against stub
// bodies; increments 7–8 wired the real read/write/TTL effects, so this now also
// asserts each op takes effect.)
func TestOps_LiveContextReturnsNilError(t *testing.T) {
	c := cache.New()
	ctx := context.Background()

	// Set returns nil and takes effect: a later Get observes the stored value.
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Errorf("Set live err = %v, want nil", err)
	}
	if got, ok, err := c.Get(ctx, "k"); err != nil || !ok || string(got) != "v" {
		t.Errorf("Get live = (%q, %v, %v), want (\"v\", true, nil) — Set did not take effect", got, ok, err)
	}

	// TTL of the live no-expiry key returns the -1 sentinel with a nil error.
	if remaining, ok, err := c.TTL(ctx, "k"); err != nil || !ok || remaining != -1 {
		t.Errorf("TTL live = (%v, %v, %v), want (-1, true, nil)", remaining, ok, err)
	}

	// Len returns nil and observes the one live entry.
	if n, err := c.Len(ctx); err != nil || n != 1 {
		t.Errorf("Len live = (%d, %v), want (1, nil)", n, err)
	}

	// Del returns nil and takes effect: existed == true, then the key reads absent.
	if existed, err := c.Del(ctx, "k"); err != nil || !existed {
		t.Errorf("Del live = (%v, %v), want (true, nil)", existed, err)
	}
	if _, ok, err := c.Get(ctx, "k"); err != nil || ok {
		t.Errorf("Get after Del live ok = %v, err = %v, want false/nil — Del did not take effect", ok, err)
	}
}
