// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Antoni Romera

package v1alpha1

import (
	"sort"
	"time"
)

// Which backups to keep.
//
// This is the grandfather-father-son policy every backup system implements and
// most implement subtly wrong. The classic mistake is treating the three rules as
// separate buckets and deleting whatever falls out of the daily one — which
// throws away the only copy that existed for a week, because it was also the
// newest that day.
//
// So the rules are UNIONS over one list, never partitions of it. A copy survives
// if any rule wants it, and a copy is deleted only when none does.
//
// The second mistake is counting backups instead of periods. "Seven daily" does
// not mean the seven newest copies: a schedule that ran hourly for a day would
// then keep seven copies of the same afternoon and nothing older. It means the
// newest copy of each of the seven most recent days that HAVE one.
//
// That last phrase is deliberate and is the third mistake avoided. Retention
// here counts periods with copies in them, not calendar days from now — so a
// schedule that stopped running a year ago still has its last seven copies
// rather than none. Ageing them out by wall clock would delete the last backup
// of a database because nothing had backed it up recently, which is the one
// outcome a backup system must never produce. Nothing is removed because time
// passed; things are removed because NEWER copies took their place.
//
// Written here rather than in the controller because it decides what gets
// deleted, and things that decide what gets deleted deserve to be testable
// without a cluster.

// Retain sorts backups into those kept and those to remove.
//
// Returns both, rather than only the ones to delete, so a caller can report what
// is being kept and why — which is the difference between a backup policy people
// trust and one they check by hand.
func Retain(copies []BackupCopy, policy BackupRetention, now time.Time) (keep, drop []BackupCopy) {
	if len(copies) == 0 {
		return nil, nil
	}

	// Newest first. Every rule below picks the newest of its period, so the
	// order is the whole algorithm.
	sorted := append([]BackupCopy(nil), copies...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TakenAt.Time.After(sorted[j].TakenAt.Time)
	})

	kept := make(map[string]bool, len(sorted))

	// One pass per rule, each taking the newest copy of each period until it has
	// as many periods as it wants.
	markNewestPerPeriod(sorted, kept, policy.Daily, dayKey)
	markNewestPerPeriod(sorted, kept, policy.Weekly, weekKey)
	markNewestPerPeriod(sorted, kept, policy.Monthly, monthKey)

	for _, c := range sorted {
		if kept[c.Name] {
			keep = append(keep, c)
		} else {
			drop = append(drop, c)
		}
	}
	return keep, drop
}

// markNewestPerPeriod keeps the newest copy of each of the `periods` most recent
// periods that have one.
func markNewestPerPeriod(sorted []BackupCopy, kept map[string]bool, periods int32, key func(time.Time) string) {
	if periods <= 0 {
		return
	}
	seen := make(map[string]bool, periods)
	for _, c := range sorted {
		k := key(c.TakenAt.Time.UTC())
		if seen[k] {
			// Not the newest of its period; some other rule may still want it.
			continue
		}
		if int32(len(seen)) >= periods { //nolint:gosec // bounded by the schema
			return
		}
		seen[k] = true
		kept[c.Name] = true
	}
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// weekKey uses the ISO week, so a week is Monday to Sunday everywhere and does
// not shift with the locale of whoever is reading.
func weekKey(t time.Time) string {
	y, w := t.ISOWeek()
	return isoWeekString(y, w)
}

func isoWeekString(y, w int) string {
	return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-W" + twoDigits(w)
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

// TierOf says which rule a backup is being kept by, for display.
//
// The most durable rule wins: a copy that is both today's and this month's is
// shown as monthly, because that is the one that will still be keeping it in
// three weeks. Showing it as daily would make it look like it is about to go.
func TierOf(c BackupCopy, policy BackupRetention, all []BackupCopy) string {
	monthly, _ := Retain(all, BackupRetention{Monthly: policy.Monthly}, time.Time{})
	if containsCopy(monthly, c.Name) {
		return "monthly"
	}
	weekly, _ := Retain(all, BackupRetention{Weekly: policy.Weekly}, time.Time{})
	if containsCopy(weekly, c.Name) {
		return "weekly"
	}
	return "daily"
}

func containsCopy(in []BackupCopy, name string) bool {
	for _, c := range in {
		if c.Name == name {
			return true
		}
	}
	return false
}
