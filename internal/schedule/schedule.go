/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package schedule holds the time-window maths behind ScalingSchedule.
//
// It never talks to the Kubernetes API, so it is tested with plain
// table-driven `go test` cases (see schedule_test.go). Keeping hard logic out
// of the reconciler like this is the easiest way to keep operators testable.
package schedule

import (
	"slices"
	"time"
)

const (
	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
	// lookahead is how many days forward we search for the next transition.
	// The schedule repeats every week, so 8 days always covers it.
	lookahead = 8
)

// Window is one recurring time range, already parsed and validated.
type Window struct {
	Name string
	// Days the window starts on. Empty means every day.
	Days []time.Weekday
	// Start and End are minutes after midnight. Start is inclusive and End is
	// exclusive. When End < Start the window crosses midnight.
	Start, End int
	Replicas   int32
}

// Schedule is a validated ScalingSchedule spec, ready to evaluate.
type Schedule struct {
	Location        *time.Location
	DefaultReplicas int32
	Windows         []Window
}

// Result is the outcome of evaluating a Schedule at a point in time.
type Result struct {
	// Window is the active window, or nil when the default applies.
	Window *Window
	// Replicas is the replica count the target should have right now.
	Replicas int32
	// NextTransition is the next instant at which the active window changes.
	// It is the zero time.Time if the schedule never changes.
	NextTransition time.Time
}

// Overlap names two windows that are active at the same time at least once
// per week.
type Overlap struct {
	First, Second string
}

// Evaluate reports which window is active at now and when that will change.
func (s *Schedule) Evaluate(now time.Time) Result {
	local := now.In(s.Location)
	res := Result{Replicas: s.DefaultReplicas}
	if i := s.activeIndex(local); i >= 0 {
		res.Window = &s.Windows[i]
		res.Replicas = s.Windows[i].Replicas
	}
	res.NextTransition = s.nextTransition(local)
	return res
}

// activeIndex returns the index of the first window active at t, or -1.
func (s *Schedule) activeIndex(t time.Time) int {
	for i := range s.Windows {
		if s.Windows[i].ActiveAt(t) {
			return i
		}
	}
	return -1
}

// nextTransition finds the earliest window boundary after now at which the
// active window actually changes.
//
// Instead of doing duration arithmetic (now + 24h is wrong across a daylight
// saving change), we build each candidate boundary with time.Date in the
// schedule's location. time.Date handles DST for us.
func (s *Schedule) nextTransition(now time.Time) time.Time {
	current := s.activeIndex(now)

	y, m, d := now.Date()
	var candidates []time.Time
	for offset := -1; offset <= lookahead; offset++ {
		for _, w := range s.Windows {
			for _, mins := range []int{w.Start, w.End} {
				c := time.Date(y, m, d+offset, mins/60, mins%60, 0, 0, s.Location)
				if c.After(now) {
					candidates = append(candidates, c)
				}
			}
		}
	}
	slices.SortFunc(candidates, time.Time.Compare)

	for _, c := range candidates {
		if s.activeIndex(c) != current {
			return c
		}
	}
	return time.Time{}
}

// Overlaps returns every pair of windows whose weekly time ranges intersect.
// Overlaps are legal (the first window wins), so callers usually turn these
// into warnings rather than errors.
func (s *Schedule) Overlaps() []Overlap {
	var out []Overlap
	for i := range s.Windows {
		a := s.Windows[i].weekIntervals()
		for j := i + 1; j < len(s.Windows); j++ {
			if intersects(a, s.Windows[j].weekIntervals()) {
				out = append(out, Overlap{First: s.Windows[i].Name, Second: s.Windows[j].Name})
			}
		}
	}
	return out
}

// ActiveAt reports whether the window covers the wall-clock time t.
// t must already be in the schedule's location.
func (w Window) ActiveAt(t time.Time) bool {
	mins := t.Hour()*60 + t.Minute()
	today := t.Weekday()

	if !w.crossesMidnight() {
		return w.startsOn(today) && mins >= w.Start && mins < w.End
	}
	// An overnight window is active if it started today and we are past the
	// start, or if it started yesterday and we are before the end.
	yesterday := (today + 6) % 7
	return (w.startsOn(today) && mins >= w.Start) ||
		(w.startsOn(yesterday) && mins < w.End)
}

func (w Window) crossesMidnight() bool { return w.End < w.Start }

func (w Window) startsOn(d time.Weekday) bool {
	return len(w.Days) == 0 || slices.Contains(w.Days, d)
}

// interval is a half-open range of minutes since Sunday 00:00.
type interval struct{ start, end int }

// weekIntervals expands the window into concrete ranges over one week.
func (w Window) weekIntervals() []interval {
	var out []interval
	for d := time.Sunday; d <= time.Saturday; d++ {
		if !w.startsOn(d) {
			continue
		}
		start := int(d)*minutesPerDay + w.Start
		end := int(d)*minutesPerDay + w.End
		if w.crossesMidnight() {
			end += minutesPerDay
		}
		if end > minutesPerWeek {
			// A Saturday-night window spills into Sunday morning.
			out = append(out, interval{start, minutesPerWeek}, interval{0, end - minutesPerWeek})
			continue
		}
		out = append(out, interval{start, end})
	}
	return out
}

func intersects(a, b []interval) bool {
	for _, x := range a {
		for _, y := range b {
			if x.start < y.end && y.start < x.end {
				return true
			}
		}
	}
	return false
}
