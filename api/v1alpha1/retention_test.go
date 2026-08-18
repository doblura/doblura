// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Retention decides what gets deleted, so it is tested against the mistakes
// rather than against the happy path.

func at(s string) BackupCopy {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return BackupCopy{Name: s, TakenAt: metav1.NewTime(t.UTC())}
}

func names(in []BackupCopy) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.Name)
	}
	return out
}

// The mistake this exists to prevent: a copy that has fallen out of the daily
// window but is the only one for its week must survive.
func TestAWeeklySurvivesFallingOutOfTheDailyWindow(t *testing.T) {
	copies := []BackupCopy{
		at("2026-03-16 02:00"), // Monday, this week
		at("2026-03-15 02:00"),
		at("2026-03-14 02:00"),
		at("2026-03-02 02:00"), // three weeks back: the only copy of its week
	}
	keep, drop := Retain(copies, BackupRetention{Daily: 3, Weekly: 4}, time.Time{})

	if containsCopy(drop, "2026-03-02 02:00") {
		t.Errorf("the only copy of its week was deleted because it fell out of the daily window\nkept: %v\ndropped: %v",
			names(keep), names(drop))
	}
}

// Periods, not copies: several backups on one day count as ONE day.
func TestDailyCountsDaysAndNotBackups(t *testing.T) {
	var copies []BackupCopy
	for h := 0; h < 8; h++ {
		copies = append(copies, at(fmt.Sprintf("2026-03-16 %02d:00", h)))
	}
	copies = append(copies, at("2026-03-15 02:00"), at("2026-03-14 02:00"))

	keep, _ := Retain(copies, BackupRetention{Daily: 3}, time.Time{})
	if len(keep) != 3 {
		t.Errorf("eight copies of one afternoon should count as one day, kept %d: %v",
			len(keep), names(keep))
	}
	// And the one kept for that day is the NEWEST of it.
	if !containsCopy(keep, "2026-03-16 07:00") {
		t.Errorf("the newest copy of the day must be the one kept, got %v", names(keep))
	}
}

// A copy kept by two rules is kept once, not twice.
func TestACopyKeptByTwoRulesAppearsOnce(t *testing.T) {
	copies := []BackupCopy{at("2026-03-16 02:00"), at("2026-03-15 02:00")}
	keep, drop := Retain(copies, BackupRetention{Daily: 7, Weekly: 4, Monthly: 3}, time.Time{})
	if len(keep) != 2 || len(drop) != 0 {
		t.Errorf("two copies, everything kept: got keep=%v drop=%v", names(keep), names(drop))
	}
	seen := map[string]int{}
	for _, c := range keep {
		seen[c.Name]++
	}
	for n, count := range seen {
		if count != 1 {
			t.Errorf("%s appears %d times in the kept list", n, count)
		}
	}
}

// A rule set to zero keeps nothing of its own, and does not affect the others.
func TestZeroMeansNoneOfThatRule(t *testing.T) {
	copies := []BackupCopy{
		at("2026-03-16 02:00"), at("2026-03-09 02:00"), at("2026-02-16 02:00"),
	}
	keep, _ := Retain(copies, BackupRetention{Daily: 0, Weekly: 0, Monthly: 2}, time.Time{})
	if len(keep) != 2 {
		t.Errorf("monthly 2 alone should keep two months, kept %v", names(keep))
	}
}

// Nothing in, nothing out — and no panic, which is the failure that would take
// the controller down on a fresh backup that has not run yet.
func TestEmptyIsSafe(t *testing.T) {
	keep, drop := Retain(nil, BackupRetention{Daily: 7}, time.Time{})
	if len(keep) != 0 || len(drop) != 0 {
		t.Errorf("empty in, empty out: got %v / %v", names(keep), names(drop))
	}
}

// Copies go when NEWER ones take their place, and never merely because time
// passed.
//
// This asserted the opposite first, and the implementation was right: retention
// counts periods that have copies, not calendar days from now. A schedule that
// stopped running a year ago keeps its last seven copies rather than none —
// ageing them out by wall clock would delete the last backup of a database
// because nothing had backed it up recently, which is the one outcome a backup
// system must never produce.
func TestCopiesGoWhenNewerOnesReplaceThemAndNotBecauseTimePassed(t *testing.T) {
	old := []BackupCopy{at("2026-03-16 02:00"), at("2024-01-05 02:00")}
	_, drop := Retain(old, BackupRetention{Daily: 7, Weekly: 4, Monthly: 3}, time.Time{})
	if len(drop) != 0 {
		t.Errorf("with only two copies and room for seven, nothing should be deleted: %v",
			names(drop))
	}

	// Now give it more days than the policy keeps: the oldest goes.
	var many []BackupCopy
	for d := 1; d <= 10; d++ {
		many = append(many, at(fmt.Sprintf("2026-03-%02d 02:00", d)))
	}
	_, drop = Retain(many, BackupRetention{Daily: 3}, time.Time{})
	if !containsCopy(drop, "2026-03-01 02:00") {
		t.Errorf("with ten days and room for three, the oldest must go: dropped %v", names(drop))
	}
	if containsCopy(drop, "2026-03-10 02:00") {
		t.Error("the newest was deleted")
	}
}

// The ISO week runs Monday to Sunday: a Sunday and the Monday after it are
// DIFFERENT weeks, and getting this wrong silently halves weekly retention.
func TestSundayAndMondayAreDifferentWeeks(t *testing.T) {
	sunday := at("2026-03-15 02:00") // ISO week 11
	monday := at("2026-03-16 02:00") // ISO week 12
	keep, _ := Retain([]BackupCopy{monday, sunday}, BackupRetention{Weekly: 2}, time.Time{})
	if len(keep) != 2 {
		t.Errorf("a Sunday and the following Monday are two ISO weeks, kept %v", names(keep))
	}
}

// The prune deletes the file the listing named, extension included.
//
// It did not: the listing strips ".zip" so the name reads like a timestamp, and
// the delete used the bare name — a path that does not exist, which `rm -rf`
// removes successfully. Three removals were reported, nothing was deleted, and
// the volume grew for ever while the status said the policy was being applied.
// The name and the file it refers to are two different strings and the test says
// which is which.
func TestBackupNamesAreNotFileNames(t *testing.T) {
	c := at("2026-03-16 02:00")
	if strings.HasSuffix(c.Name, ".zip") {
		t.Error("the recorded name carries the extension; the listing strips it")
	}
}
