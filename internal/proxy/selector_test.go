package proxy

import "testing"

func TestPickExcluding_SkipsDownAndExcluded(t *testing.T) {
	a := newFake("a", StatusUp, 10)
	down := newFake("down", StatusDown, 1)
	c := newFake("c", StatusUp, 20)

	// down must never be picked even though it has the lowest EWMA.
	for i := 0; i < 50; i++ {
		got := PickExcluding([]upstream{a, down, c}, nil)
		if got == down {
			t.Fatal("picked a down upstream")
		}
	}

	// Excluding a leaves only c (down is skipped).
	got := PickExcluding([]upstream{a, down, c}, map[upstream]bool{a: true})
	if got != c {
		t.Fatalf("want c, got %v", got)
	}
}

func TestPickExcluding_AllDownOrExcluded(t *testing.T) {
	a := newFake("a", StatusUp, 10)
	b := newFake("b", StatusDown, 10)
	if got := PickExcluding([]upstream{a, b}, map[upstream]bool{a: true}); got != nil {
		t.Fatalf("want nil when all up are excluded, got %v", got)
	}
	if got := PickExcluding([]upstream{b}, nil); got != nil {
		t.Fatalf("want nil when all down, got %v", got)
	}
}

func TestPickExcluding_LowerEWMAWins(t *testing.T) {
	fast := newFake("fast", StatusUp, 5)
	slow := newFake("slow", StatusUp, 500)
	// With only two candidates, P2C always compares both → fast must win every time.
	for i := 0; i < 50; i++ {
		if got := PickExcluding([]upstream{fast, slow}, nil); got != fast {
			t.Fatalf("want fast, got %v", got)
		}
	}
}

func TestPick_SingleCandidate(t *testing.T) {
	only := newFake("only", StatusUp, 99)
	if got := Pick([]upstream{only}); got != only {
		t.Fatalf("want only, got %v", got)
	}
}

func newBackupFake(id string, status Status, ewma float64) *fakeUpstream {
	f := newFake(id, status, ewma)
	f.backup = true
	return f
}

func TestPick_PrefersPrimaryOverBackup(t *testing.T) {
	primary := newFake("primary", StatusUp, 500)      // slow primary
	backup := newBackupFake("backup", StatusUp, 1)    // fast backup, must be ignored
	// A healthy primary always wins, even when a backup has a far lower EWMA.
	for i := 0; i < 50; i++ {
		if got := Pick([]upstream{primary, backup}); got != primary {
			t.Fatalf("want primary while it is up, got %v", got)
		}
	}
}

func TestPick_FallsBackToBackupWhenAllPrimariesDown(t *testing.T) {
	primary := newFake("primary", StatusDown, 1)
	backup := newBackupFake("backup", StatusUp, 500)
	for i := 0; i < 50; i++ {
		if got := Pick([]upstream{primary, backup}); got != backup {
			t.Fatalf("want backup when all primaries are down, got %v", got)
		}
	}
}

func TestPick_NilWhenPrimaryAndBackupDown(t *testing.T) {
	primary := newFake("primary", StatusDown, 1)
	backup := newBackupFake("backup", StatusDown, 1)
	if got := Pick([]upstream{primary, backup}); got != nil {
		t.Fatalf("want nil when every tier is down, got %v", got)
	}
}
