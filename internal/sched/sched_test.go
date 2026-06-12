package sched

import (
	"testing"
	"time"
)

func at(weekday time.Weekday, hour, min int) time.Time {
	// 2026-06-01 is a Monday.
	t := time.Date(2026, 6, 1, hour, min, 0, 0, time.UTC)
	return t.AddDate(0, 0, int(weekday-t.Weekday()+7)%7)
}

func TestWeekdayMornings(t *testing.T) {
	e, err := Parse("0 9 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Matches(at(time.Monday, 9, 0)) {
		t.Error("should fire Monday 09:00")
	}
	if e.Matches(at(time.Saturday, 9, 0)) {
		t.Error("should not fire Saturday")
	}
	if e.Matches(at(time.Monday, 9, 1)) {
		t.Error("should not fire at 09:01")
	}
}

func TestSteps(t *testing.T) {
	e, err := Parse("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []int{0, 15, 30, 45} {
		if !e.Matches(at(time.Monday, 3, m)) {
			t.Errorf("should fire at :%02d", m)
		}
	}
	if e.Matches(at(time.Monday, 3, 7)) {
		t.Error("should not fire at :07")
	}
}

func TestListsAndSundaySeven(t *testing.T) {
	e, err := Parse("30 8,18 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if !e.Matches(at(time.Sunday, 8, 30)) || !e.Matches(at(time.Sunday, 18, 30)) {
		t.Error("dow=7 should mean Sunday")
	}
	if e.Matches(at(time.Monday, 8, 30)) {
		t.Error("should not fire Monday")
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "61 * * * *", "* 25 * * *", "x * * * *", "*/0 * * * *"} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("expected error for %q", expr)
		}
	}
}
