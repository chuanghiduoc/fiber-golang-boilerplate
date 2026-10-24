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
			KeyEmailVerification: {Enabled: true},
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
			snapshot:   Snapshot{Flags: map[Key]FlagState{KeyEmailVerification: {Enabled: true}}},
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
	snapshot.Flags[KeyEmailVerification] = FlagState{Enabled: true, KillSwitch: true}
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
		Flags: map[Key]FlagState{KeyUserRegistration: {Enabled: true}},
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
		Flags:         map[Key]FlagState{KeyEmailVerification: {Enabled: true}},
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

	// An immediate retry is throttled and must not hit the store again.
	loads := store.loads
	_ = evaluator.Enabled(context.Background(), KeyEmailVerification, Subject{TenantID: 1, UserID: 42})
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
		return Snapshot{Flags: map[Key]FlagState{KeyUserRegistration: {Enabled: false}}}, nil
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
