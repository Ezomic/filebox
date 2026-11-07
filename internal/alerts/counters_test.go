package alerts

import (
	"testing"
	"time"
)

func TestCounterTakesTheFirstReadingAsItsBaseline(t *testing.T) {
	counter := observe(Counter{}, false, 8, start)
	if counter.Count != 8 || counter.From != 8 || counter.Rising(start) {
		t.Fatalf("got %+v", counter)
	}
}

func TestCounterRisesAndSettlesAfterTheWindow(t *testing.T) {
	counter := observe(Counter{}, false, 0, start)
	counter = observe(counter, true, 3, start.Add(time.Hour))
	if !counter.Rising(start.Add(time.Hour)) || counter.From != 0 || counter.Count != 3 {
		t.Fatalf("got %+v", counter)
	}
	counter = observe(counter, true, 5, start.Add(48*time.Hour))
	if counter.From != 0 || counter.Count != 5 || !counter.RoseAt.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("rises close together are one rise from the old baseline: %+v", counter)
	}
	settled := start.Add(48*time.Hour + RiseWindow)
	if !counter.Rising(settled.Add(-time.Second)) || counter.Rising(settled) {
		t.Fatalf("a rise lasts exactly RiseWindow: %+v", counter)
	}
	if observe(counter, true, 5, settled) != counter || counter.Rising(settled) {
		t.Fatalf("a steady count settles: %+v", counter)
	}
	counter = observe(counter, true, 6, settled.Add(time.Hour))
	if counter.From != 5 || !counter.Rising(settled.Add(time.Hour)) {
		t.Fatalf("a later rise starts from the settled count: %+v", counter)
	}
}

func TestCounterIgnoresALowReadingThatDoesNotLast(t *testing.T) {
	counter := observe(Counter{}, false, 3, start)
	counter = observe(counter, true, 9, start.Add(time.Hour))
	risen := counter
	for i, reading := range []uint64{0, 0, 9, 0, 9} {
		at := start.Add(time.Duration(2+i) * time.Hour)
		counter = observe(counter, true, reading, at)
		if counter.Count != 9 || counter.From != 3 || !counter.RoseAt.Equal(risen.RoseAt) || !counter.Rising(at) {
			t.Fatalf("reading %d at %d: a flaky zero must not touch the rise: %+v", reading, i, counter)
		}
	}
	if !counter.LowSince.IsZero() {
		t.Fatalf("a normal reading ends the low spell: %+v", counter)
	}
	if counter = observe(counter, true, 10, start.Add(8*time.Hour)); counter.From != 3 || counter.Count != 10 {
		t.Fatalf("a real rise after a flaky zero still counts from the baseline: %+v", counter)
	}
}

func TestCounterTakesALowerCountAsTheNewBaselineAfterADay(t *testing.T) {
	counter := observe(Counter{}, false, 0, start)
	counter = observe(counter, true, 9, start.Add(time.Hour))
	low := start.Add(2 * time.Hour)
	counter = observe(counter, true, 0, low)
	counter = observe(counter, true, 1, low.Add(LowerAfter-time.Minute))
	if counter.Count != 9 || !counter.LowSince.Equal(low) {
		t.Fatalf("not lower long enough yet: %+v", counter)
	}
	counter = observe(counter, true, 1, low.Add(LowerAfter))
	if counter != (Counter{Count: 1, From: 1}) || counter.Rising(low.Add(LowerAfter)) {
		t.Fatalf("a reset or a replaced disk becomes the baseline: %+v", counter)
	}
}

func TestCountersSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	counters, err := OpenCounters(dir)
	if err != nil {
		t.Fatal(err)
	}
	counters.Observe("reallocated:A", 2, start)
	counters.Observe("reallocated:A", 4, start.Add(time.Hour))

	reopened, err := OpenCounters(dir)
	if err != nil {
		t.Fatal(err)
	}
	counter, found := reopened.Get("reallocated:A")
	if !found || counter.Count != 4 || counter.From != 2 || !counter.Rising(start.Add(2*time.Hour)) {
		t.Fatalf("got %+v %v", counter, found)
	}
	if again := reopened.Observe("reallocated:A", 4, start.Add(3*time.Hour)); again.From != 2 || !again.RoseAt.Equal(start.Add(time.Hour)) {
		t.Fatalf("a restart must not take the current count as a new baseline: %+v", again)
	}
}
