package unbounded

import (
	"testing"
)

func TestRandomSTUNBatch_EmptyPoolReturnsNothing(t *testing.T) {
	batch, err := randomSTUNBatch(nil)(5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batch) != 0 {
		t.Errorf("want empty batch from empty pool, got %v", batch)
	}
}

func TestRandomSTUNBatch_ZeroSize(t *testing.T) {
	batch, err := randomSTUNBatch([]string{"a", "b", "c"})(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batch) != 0 {
		t.Errorf("size=0 should produce empty batch, got %v", batch)
	}
}

func TestRandomSTUNBatch_SizeBiggerThanPool(t *testing.T) {
	pool := []string{"a", "b", "c"}
	batch, err := randomSTUNBatch(pool)(10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batch) != len(pool) {
		t.Errorf("want batch len = pool len (%d), got %d", len(pool), len(batch))
	}
	if !isPermutation(batch, pool) {
		t.Errorf("batch should be a permutation of the pool; got %v vs %v", batch, pool)
	}
}

func TestRandomSTUNBatch_UniqueEntries(t *testing.T) {
	pool := []string{"a", "b", "c", "d", "e"}
	batch, err := randomSTUNBatch(pool)(3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("want 3 entries, got %d", len(batch))
	}
	seen := map[string]bool{}
	for _, s := range batch {
		if seen[s] {
			t.Errorf("batch contains duplicate %q (full batch: %v)", s, batch)
		}
		seen[s] = true
	}
}

func TestRandomSTUNBatch_DoesNotMutatePool(t *testing.T) {
	pool := []string{"a", "b", "c", "d", "e"}
	original := append([]string{}, pool...)
	sampler := randomSTUNBatch(pool)
	// Mutate the input after construction — should have no effect.
	pool[0] = "MUTATED"
	// Call twice to ensure internal state isn't draining on each call.
	for i := 0; i < 2; i++ {
		batch, err := sampler(2)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		for _, s := range batch {
			if s == "MUTATED" {
				t.Errorf("sampler leaked post-construction mutation: %v", batch)
			}
			found := false
			for _, orig := range original {
				if s == orig {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("sampler returned %q which isn't in the original pool %v", s, original)
			}
		}
	}
}

func isPermutation(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, v := range counts {
		if v != 0 {
			return false
		}
	}
	return true
}
