package features

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCatalogContainsKnownKeys(t *testing.T) {
	defs := Catalog()
	if len(defs) == 0 {
		t.Fatal("catalog must not be empty")
	}
	seen := make(map[Key]struct{}, len(defs))
	for _, def := range defs {
		if def.Key == "" {
			t.Fatal("catalog contains an empty key")
		}
		if _, duplicate := seen[def.Key]; duplicate {
			t.Fatalf("catalog contains duplicate key %q", def.Key)
		}
		seen[def.Key] = struct{}{}
		if !IsKnown(def.Key) {
			t.Fatalf("IsKnown(%q) = false", def.Key)
		}
	}

	if !CompiledDefault(KeyUserRegistration) {
		t.Error("user_registration must default to enabled")
	}
	if CompiledDefault(KeyEmailVerification) {
		t.Error("email_verification must default to disabled")
	}
	if CompiledDefault(Key("removed_flag")) {
		t.Error("unknown flags must default to disabled")
	}
}

func TestCatalogReturnsCopy(t *testing.T) {
	defs := Catalog()
	defs[0].Key = "mutated"
	if !IsKnown(KeyUserRegistration) {
		t.Error("mutating Catalog() must not alter the shared catalog")
	}
}

func TestParseFlags(t *testing.T) {
	got, err := ParseFlags(" user_registration=true, email_verification=false ,,file_uploads ")
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(ParseFlags) = %d, want 3", len(got))
	}
	if !got[KeyUserRegistration] || got[KeyEmailVerification] || !got[KeyFileUploads] {
		t.Fatalf("unexpected parsed values: %+v", got)
	}

	empty, err := ParseFlags("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("ParseFlags(empty) = %v, %v", empty, err)
	}
}

func TestParseFlagsRejectsMalformedInput(t *testing.T) {
	cases := []string{
		"=true",
		"user_registration=",
		"user_registration=maybe",
		"user_registration=true,user_registration=false",
	}
	for _, raw := range cases {
		if _, err := ParseFlags(raw); err == nil {
			t.Errorf("ParseFlags(%q) should fail", raw)
		}
	}
}

func TestParseForceOff(t *testing.T) {
	got, err := ParseForceOff("user_registration, google_oauth,")
	if err != nil {
		t.Fatalf("ParseForceOff: %v", err)
	}
	if _, ok := got[KeyUserRegistration]; !ok {
		t.Error("user_registration missing from force-off set")
	}
	if _, ok := got[KeyGoogleOAuth]; !ok {
		t.Error("google_oauth missing from force-off set")
	}
	if _, err := ParseForceOff("user_registration,user_registration"); err == nil {
		t.Error("duplicate force-off keys must fail")
	}
}

func TestKnownKeysError(t *testing.T) {
	if err := KnownKeysError(map[Key]bool{KeyFileUploads: true}); err != nil {
		t.Errorf("known key rejected: %v", err)
	}
	if err := KnownKeysError(map[Key]bool{"typo": true}); err == nil {
		t.Error("unknown key must be rejected")
	}
	if err := KnownForceOffError(map[Key]struct{}{"typo": {}}); err == nil {
		t.Error("unknown force-off key must be rejected")
	}
}

type fakeStore struct {
	snapshot Snapshot
	err      error
	loads    int
}

func (s *fakeStore) LoadSnapshot(context.Context) (Snapshot, error) {
	s.loads++
	if s.err != nil {
		return Snapshot{}, s.err
	}
	return s.snapshot, nil
}

func testSnapshot() Snapshot {
	return Snapshot{
		Flags: map[Key]FlagState{
			KeyEmailVerification: {Enabled: boolPtr(true)},
		},
		TenantTargets: map[Key]map[int64]bool{
			KeyEmailVerification: {1: false},
		},
		UserTargets: map[Key]map[UserTargetKey]bool{
			KeyEmailVerification: {
				{TenantID: 1, UserID: 42}: true,
			},
		},
	}
}

func TestEvaluatorPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		key        Key
		subject    Subject
		env        map[Key]bool
		forceOff   map[Key]struct{}
		snapshot   Snapshot
		want       bool
		wantSource Source
	}{
		{
			name:       "force-off wins over everything",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1, UserID: 42},
			env:        map[Key]bool{KeyEmailVerification: true},
			forceOff:   map[Key]struct{}{KeyEmailVerification: {}},
			snapshot:   testSnapshot(),
			want:       false,
			wantSource: SourceForceOff,
		},
		{
			name:       "kill switch beats user target",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1, UserID: 42},
			snapshot:   killSwitchSnapshot(),
			want:       false,
			wantSource: SourceKillSwitch,
		},
		{
			name:       "user target beats tenant target",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1, UserID: 42},
			snapshot:   testSnapshot(),
			want:       true,
			wantSource: SourceUserTarget,
		},
		{
			name:       "tenant target beats database default",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1, UserID: 7},
			snapshot:   testSnapshot(),
			want:       false,
			wantSource: SourceTenantTarget,
		},
		{
			name:       "database default beats environment",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1},
			env:        map[Key]bool{KeyEmailVerification: false},
			snapshot:   Snapshot{Flags: map[Key]FlagState{KeyEmailVerification: {Enabled: boolPtr(true)}}},
			want:       true,
			wantSource: SourceDatabaseDefault,
		},
		{
			name:       "environment beats compiled default",
			key:        KeyEmailVerification,
			subject:    Subject{TenantID: 1},
			env:        map[Key]bool{KeyEmailVerification: true},
			snapshot:   Snapshot{},
			want:       true,
			wantSource: SourceEnvironment,
		},
		{
			name:       "compiled default is last",
			key:        KeyUserRegistration,
			subject:    Subject{TenantID: 1},
			snapshot:   Snapshot{},
			want:       true,
			wantSource: SourceCompiledDefault,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{snapshot: tt.snapshot}
			evaluator := NewEvaluator(Options{
				Store:    store,
				Env:      tt.env,
				ForceOff: tt.forceOff,
			})
			got := evaluator.Evaluate(context.Background(), tt.key, tt.subject)
			if got.Enabled != tt.want || got.Source != tt.wantSource {
				t.Fatalf("Evaluate() = %+v, want enabled=%v source=%v", got, tt.want, tt.wantSource)
			}
		})
	}
}

func killSwitchSnapshot() Snapshot {
	snapshot := testSnapshot()
	snapshot.Flags[KeyEmailVerification] = FlagState{Enabled: boolPtr(true), KillSwitch: true}
	return snapshot
}

func TestEvaluatorUserTargetIsTenantScoped(t *testing.T) {
	store := &fakeStore{snapshot: testSnapshot()}
	evaluator := NewEvaluator(Options{Store: store})

	// The same user ID in another tenant must not inherit tenant 1's target.
	got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 2, UserID: 42})
	if !got.Enabled || got.Source != SourceDatabaseDefault {
		t.Fatalf("cross-tenant user target = %+v, want database default true", got)
	}
}

func TestEvaluatorDisabledUserTargetOverridesEnabledDefault(t *testing.T) {
	snapshot := Snapshot{
		Flags: map[Key]FlagState{KeyUserRegistration: {Enabled: boolPtr(true)}},
		UserTargets: map[Key]map[UserTargetKey]bool{
			KeyUserRegistration: {{TenantID: 1, UserID: 9}: false},
		},
	}
	evaluator := NewEvaluator(Options{Store: &fakeStore{snapshot: snapshot}})
	got := evaluator.Evaluate(context.Background(), KeyUserRegistration, Subject{TenantID: 1, UserID: 9})
	if got.Enabled || got.Source != SourceUserTarget {
		t.Fatalf("disabled user target = %+v, want false/user_target", got)
	}
}

func TestEvaluatorZeroSubjectUsesContextTenant(t *testing.T) {
	snapshot := Snapshot{
		Flags:         map[Key]FlagState{KeyEmailVerification: {Enabled: boolPtr(true)}},
		TenantTargets: map[Key]map[int64]bool{KeyEmailVerification: {1: false}},
	}
	evaluator := NewEvaluator(Options{Store: &fakeStore{snapshot: snapshot}})
	got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{})
	if got.Enabled || got.Source != SourceTenantTarget {
		t.Fatalf("zero subject = %+v, want default tenant target false", got)
	}
}

func TestEvaluatorCacheTTLAndInvalidation(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{snapshot: testSnapshot()}
	evaluator := NewEvaluator(Options{
		Store:        store,
		CacheTTL:     10 * time.Second,
		MaxStaleness: 60 * time.Second,
		Now:          func() time.Time { return now },
	})

	if !evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42}) {
		t.Fatal("initial evaluation should use the snapshot user target")
	}
	_ = evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	if store.loads != 1 {
		t.Fatalf("store loads = %d, want 1 within TTL", store.loads)
	}

	now = now.Add(11 * time.Second)
	_ = evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	waitForRefresh(evaluator)
	if store.loads != 2 {
		t.Fatalf("store loads = %d, want 2 after TTL", store.loads)
	}

	if err := evaluator.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	_ = evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	if store.loads != 3 {
		t.Fatalf("store loads = %d, want 3 after invalidation", store.loads)
	}
}

func TestEvaluatorServesStaleSnapshotWithinMaxStaleness(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{snapshot: testSnapshot()}
	evaluator := NewEvaluator(Options{
		Store:        store,
		CacheTTL:     10 * time.Second,
		MaxStaleness: 60 * time.Second,
		Env:          map[Key]bool{KeyEmailVerification: false},
		Now:          func() time.Time { return now },
	})

	if !evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42}) {
		t.Fatal("initial evaluation should use user target")
	}

	now = now.Add(11 * time.Second)
	store.err = errors.New("database down")
	got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	if !got.Enabled || got.Source != SourceUserTarget {
		t.Fatalf("stale evaluation = %+v, want user target from stale snapshot", got)
	}
	waitForRefresh(evaluator)

	// An immediate retry is throttled and must not hit the store again.
	loads := store.loads
	_ = evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	waitForRefresh(evaluator)
	if store.loads != loads {
		t.Fatalf("failed snapshot load was not throttled: loads %d -> %d", loads, store.loads)
	}

	now = now.Add(61 * time.Second)
	got = evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
	if got.Enabled || got.Source != SourceEnvironment {
		t.Fatalf("evaluation after max staleness = %+v, want environment fallback false", got)
	}
}

func TestEvaluatorFallsBackWithoutStore(t *testing.T) {
	evaluator := NewEvaluator(Options{Env: map[Key]bool{KeyUserRegistration: false}})
	got := evaluator.Evaluate(context.Background(), KeyUserRegistration, Subject{})
	if got.Enabled || got.Source != SourceEnvironment {
		t.Fatalf("no-store evaluation = %+v, want environment false", got)
	}
	got = evaluator.Evaluate(context.Background(), KeyFileUploads, Subject{})
	if !got.Enabled || got.Source != SourceCompiledDefault {
		t.Fatalf("no-store compiled evaluation = %+v, want true/compiled", got)
	}
}

func TestEvaluatorInvalidationDropsInFlightSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	store := StoreFunc(func(context.Context) (Snapshot, error) {
		startedOnce.Do(func() { close(started) })
		<-release
		return Snapshot{Flags: map[Key]FlagState{KeyUserRegistration: {Enabled: boolPtr(false)}}}, nil
	})
	evaluator := NewEvaluator(Options{Store: store, CacheTTL: time.Minute})

	done := make(chan Result, 1)
	go func() {
		done <- evaluator.Evaluate(context.Background(), KeyUserRegistration, Subject{})
	}()

	<-started
	// Invalidate while the first load is blocked. The stale result must be
	// discarded and the evaluator must reload a fresh snapshot.
	if err := evaluator.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	close(release)

	// The store closes "started" on the retry too; close only once is not
	// possible, so the retry must tolerate an already-closed channel.
	select {
	case got := <-done:
		if got.Enabled {
			t.Fatalf("result after invalidation = %+v, want disabled snapshot", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("evaluation did not finish")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestEvaluatorRowWithoutDatabaseDefaultFallsThrough(t *testing.T) {
	// A row created only to anchor tenant 2's target must not force a
	// database default on tenant 1.
	snapshot := Snapshot{
		Flags:         map[Key]FlagState{KeyGoogleOAuth: {}},
		TenantTargets: map[Key]map[int64]bool{KeyGoogleOAuth: {2: true}},
	}
	evaluator := NewEvaluator(Options{
		Store: &fakeStore{snapshot: snapshot},
		Env:   map[Key]bool{KeyGoogleOAuth: false},
	})

	got := evaluator.Evaluate(context.Background(), KeyGoogleOAuth, Subject{TenantID: 1})
	if got.Enabled || got.Source != SourceEnvironment {
		t.Fatalf("tenant without target = %+v, want environment false", got)
	}
	got = evaluator.Evaluate(context.Background(), KeyGoogleOAuth, Subject{TenantID: 2})
	if !got.Enabled || got.Source != SourceTenantTarget {
		t.Fatalf("targeted tenant = %+v, want tenant target true", got)
	}
}

func TestEvaluatorTargetsApplyWithoutFlagRow(t *testing.T) {
	snapshot := Snapshot{
		UserTargets: map[Key]map[UserTargetKey]bool{
			KeyFileUploads: {{TenantID: 3, UserID: 5}: false},
		},
	}
	evaluator := NewEvaluator(Options{Store: &fakeStore{snapshot: snapshot}})
	got := evaluator.Evaluate(context.Background(), KeyFileUploads, Subject{TenantID: 3, UserID: 5})
	if got.Enabled || got.Source != SourceUserTarget {
		t.Fatalf("user target without row = %+v, want false/user_target", got)
	}
	got = evaluator.Evaluate(context.Background(), KeyFileUploads, Subject{TenantID: 3, UserID: 6})
	if !got.Enabled || got.Source != SourceCompiledDefault {
		t.Fatalf("untargeted user = %+v, want compiled default true", got)
	}
}

func TestEvaluatorLoadIsDetachedFromRequestCancellation(t *testing.T) {
	var sawErr error
	var sawDeadline bool
	store := StoreFunc(func(ctx context.Context) (Snapshot, error) {
		sawErr = ctx.Err()
		_, sawDeadline = ctx.Deadline()
		return killSwitchSnapshot(), nil
	})
	evaluator := NewEvaluator(Options{Store: store})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := evaluator.Evaluate(ctx, KeyEmailVerification, Subject{TenantID: 1})
	if sawErr != nil {
		t.Fatalf("load context error = %v, want detached from request cancellation", sawErr)
	}
	if !sawDeadline {
		t.Fatal("load context must carry a bounded timeout")
	}
	if got.Source != SourceKillSwitch {
		t.Fatalf("Evaluate() = %+v, want kill switch from the loaded snapshot", got)
	}
}

func TestEvaluatorCanceledLoadIsNotThrottled(t *testing.T) {
	store := &fakeStore{snapshot: killSwitchSnapshot(), err: context.Canceled}
	evaluator := NewEvaluator(Options{Store: store, CacheTTL: time.Minute})

	_ = evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1})
	store.err = nil
	got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1})
	if store.loads != 2 {
		t.Fatalf("store loads = %d, want an immediate retry after a canceled load", store.loads)
	}
	if got.Source != SourceKillSwitch {
		t.Fatalf("Evaluate() = %+v, want kill switch after retry", got)
	}
}

func TestEvaluatorInvalidateKeepsSnapshotAsFallback(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{snapshot: killSwitchSnapshot()}
	evaluator := NewEvaluator(Options{
		Store:        store,
		CacheTTL:     10 * time.Second,
		MaxStaleness: 60 * time.Second,
		Env:          map[Key]bool{KeyEmailVerification: true},
		Now:          func() time.Time { return now },
	})

	if got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1}); got.Source != SourceKillSwitch {
		t.Fatalf("initial evaluation = %+v, want kill switch", got)
	}
	if err := evaluator.Invalidate(context.Background()); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	// The reload after invalidation fails: the kill switch must stay in force
	// instead of failing open to the environment value.
	store.err = errors.New("database down")
	got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1})
	if got.Enabled || got.Source != SourceKillSwitch {
		t.Fatalf("evaluation after invalidate+failure = %+v, want stale kill switch", got)
	}
	if store.loads != 2 {
		t.Fatalf("store loads = %d, want a reload attempt after invalidation", store.loads)
	}

	// Once the database recovers and the failure throttle expires, the
	// invalidated snapshot is replaced.
	store.err = nil
	store.snapshot = Snapshot{}
	now = now.Add(11 * time.Second)
	got = evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1})
	if !got.Enabled || got.Source != SourceEnvironment {
		t.Fatalf("evaluation after recovery = %+v, want environment true", got)
	}
}

// waitForRefresh blocks until any background snapshot refresh has finished.
// Evaluate acquires refreshMu before starting the background load, and the
// load holds it until it completes, so locking here waits for that load.
func waitForRefresh(evaluator Evaluator) {
	cached := evaluator.(*CachedEvaluator)
	for !cached.refreshMu.TryLock() {
		time.Sleep(time.Millisecond)
	}
	cached.refreshMu.Unlock()
}

func TestEvaluatorServesStaleSnapshotWhileSlowReloadRuns(t *testing.T) {
	var (
		mu    sync.Mutex
		now   = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
		loads int
	)
	release := make(chan struct{})
	store := StoreFunc(func(ctx context.Context) (Snapshot, error) {
		mu.Lock()
		loads++
		first := loads == 1
		mu.Unlock()
		if first {
			return killSwitchSnapshot(), nil
		}
		// A slow database: the reload blocks until released or timed out.
		select {
		case <-release:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
		return Snapshot{}, nil
	})
	evaluator := NewEvaluator(Options{
		Store:        store,
		CacheTTL:     10 * time.Second,
		MaxStaleness: 60 * time.Second,
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
	})

	if got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1}); got.Source != SourceKillSwitch {
		t.Fatalf("initial evaluation = %+v, want kill switch", got)
	}
	mu.Lock()
	now = now.Add(11 * time.Second)
	mu.Unlock()

	// Concurrent callers after the TTL must all get the stale snapshot
	// without waiting for the slow reload.
	const callers = 20
	var wg sync.WaitGroup
	results := make(chan Result, callers)
	start := time.Now()
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1})
		}()
	}
	wg.Wait()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stale callers took %v; they must not wait for the reload", elapsed)
	}
	close(results)
	for got := range results {
		if got.Source != SourceKillSwitch {
			t.Fatalf("stale evaluation = %+v, want kill switch from the stale snapshot", got)
		}
	}

	close(release)
	waitForRefresh(evaluator)
	mu.Lock()
	gotLoads := loads
	mu.Unlock()
	if gotLoads != 2 {
		t.Fatalf("store loads = %d, want exactly one background reload", gotLoads)
	}
	if got := evaluator.Evaluate(context.Background(), KeyEmailVerification, Subject{TenantID: 1}); got.Source == SourceKillSwitch {
		t.Fatalf("evaluation after background reload = %+v, want the reloaded snapshot", got)
	}
}
