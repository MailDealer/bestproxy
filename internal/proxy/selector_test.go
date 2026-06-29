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
