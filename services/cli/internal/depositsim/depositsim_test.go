package depositsim

import (
	"testing"
	"time"
)

var fixedNow = time.Date(2026, time.October, 19, 12, 0, 0, 0, time.UTC)

func TestResolve_OnTime_DefaultsToFutureExpectedAt(t *testing.T) {
	got := Resolve(Options{Now: fixedNow, Late: false})

	if got.MarkLate {
		t.Errorf("on-time deposit must not set MarkLate")
	}
	wantUnix := fixedNow.Add(DefaultExpectedIn).Unix()
	if got.ExpectedAtUnix != wantUnix {
		t.Errorf("ExpectedAtUnix = %d, want %d (now + DefaultExpectedIn)", got.ExpectedAtUnix, wantUnix)
	}
	if got.ExpectedAtUnix <= fixedNow.Unix() {
		t.Errorf("on-time deposit's expected_at_unix %d must be after now %d", got.ExpectedAtUnix, fixedNow.Unix())
	}
}

func TestResolve_Late_BackdatesExpectedAtAndSetsMarkLate(t *testing.T) {
	got := Resolve(Options{Now: fixedNow, Late: true})

	if !got.MarkLate {
		t.Errorf("--late deposit must set MarkLate")
	}
	wantUnix := fixedNow.Add(-DefaultLateBy).Unix()
	if got.ExpectedAtUnix != wantUnix {
		t.Errorf("ExpectedAtUnix = %d, want %d (now - DefaultLateBy)", got.ExpectedAtUnix, wantUnix)
	}
	if got.ExpectedAtUnix >= fixedNow.Unix() {
		t.Errorf("late deposit's expected_at_unix %d must be before now %d so it is already overdue", got.ExpectedAtUnix, fixedNow.Unix())
	}
}

func TestResolve_HonorsCustomExpectedIn(t *testing.T) {
	custom := 6 * time.Hour
	got := Resolve(Options{Now: fixedNow, Late: false, ExpectedIn: custom})

	want := fixedNow.Add(custom).Unix()
	if got.ExpectedAtUnix != want {
		t.Errorf("ExpectedAtUnix = %d, want %d", got.ExpectedAtUnix, want)
	}
}

func TestResolve_HonorsCustomLateBy(t *testing.T) {
	custom := 72 * time.Hour
	got := Resolve(Options{Now: fixedNow, Late: true, LateBy: custom})

	want := fixedNow.Add(-custom).Unix()
	if got.ExpectedAtUnix != want {
		t.Errorf("ExpectedAtUnix = %d, want %d", got.ExpectedAtUnix, want)
	}
}

func TestResolve_NonPositiveDurationsFallBackToDefaults(t *testing.T) {
	got := Resolve(Options{Now: fixedNow, Late: false, ExpectedIn: -time.Hour})
	want := fixedNow.Add(DefaultExpectedIn).Unix()
	if got.ExpectedAtUnix != want {
		t.Errorf("ExpectedAtUnix = %d, want default-based %d", got.ExpectedAtUnix, want)
	}

	gotLate := Resolve(Options{Now: fixedNow, Late: true, LateBy: 0})
	wantLate := fixedNow.Add(-DefaultLateBy).Unix()
	if gotLate.ExpectedAtUnix != wantLate {
		t.Errorf("ExpectedAtUnix = %d, want default-based %d", gotLate.ExpectedAtUnix, wantLate)
	}
}
