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

package schedule

// These are ordinary Go tests: no Ginkgo, no envtest, no cluster.
// Run just this package with:  go test ./internal/schedule/ -v

import (
	"testing"
	"time"

	// Embed the time zone database so the tests don't depend on the
	// machine's /usr/share/zoneinfo. main.go does the same.
	_ "time/tzdata"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

// mustSchedule is a test helper. t.Helper() makes failures point at the
// caller's line instead of this one.
func mustSchedule(t *testing.T, spec platformv1alpha1.ScalingScheduleSpec) *Schedule {
	t.Helper()
	s, errs := FromSpec(spec)
	if len(errs) > 0 {
		t.Fatalf("FromSpec() unexpected errors: %v", errs)
	}
	return s
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func officeSchedule(t *testing.T) *Schedule {
	return mustSchedule(t, platformv1alpha1.ScalingScheduleSpec{
		TimeZone:        "Europe/London",
		DefaultReplicas: 2,
		Windows: []platformv1alpha1.ScheduleWindow{
			{Name: "business", Days: []platformv1alpha1.Weekday{"Mon", "Tue", "Wed", "Thu", "Fri"}, Start: "08:00", End: "18:00", Replicas: 10},
			{Name: "nightly-batch", Start: "22:00", End: "02:00", Replicas: 4},
		},
	})
}

// TestEvaluate is a table-driven test, the most common Go testing style:
// a slice of anonymous structs, one per case, and a loop running t.Run.
func TestEvaluate(t *testing.T) {
	london := mustLoad(t, "Europe/London")
	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, time.October, day, hour, minute, 0, 0, london)
	}

	tests := []struct {
		name         string
		now          time.Time
		wantWindow   string // "" means no window (default replicas)
		wantReplicas int32
		wantNext     time.Time
	}{
		{"weekday inside business hours", at(16, 9, 0), "business", 10, at(16, 18, 0)},
		{"weekday evening falls back to default", at(16, 19, 0), "", 2, at(16, 22, 0)},
		{"overnight window after midnight", at(17, 1, 30), "nightly-batch", 4, at(17, 2, 0)},
		{"start is inclusive", at(19, 8, 0), "business", 10, at(19, 18, 0)},
		{"end is exclusive", at(19, 18, 0), "", 2, at(19, 22, 0)},
		{"weekend has no business hours", at(17, 12, 0), "", 2, at(17, 22, 0)},
		{"seconds past the start still count", at(16, 8, 0).Add(30 * time.Second), "business", 10, at(16, 18, 0)},
	}

	s := officeSchedule(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Evaluate(tc.now)

			gotWindow := ""
			if got.Window != nil {
				gotWindow = got.Window.Name
			}
			if gotWindow != tc.wantWindow {
				t.Errorf("window = %q, want %q", gotWindow, tc.wantWindow)
			}
			if got.Replicas != tc.wantReplicas {
				t.Errorf("replicas = %d, want %d", got.Replicas, tc.wantReplicas)
			}
			// Compare instants with Equal, never ==: two time.Time values for
			// the same instant can differ in location and monotonic reading.
			if !got.NextTransition.Equal(tc.wantNext) {
				t.Errorf("next transition = %v, want %v", got.NextTransition, tc.wantNext)
			}
		})
	}
}

// TestEvaluateAcrossDaylightSaving shows why we build boundaries with
// time.Date instead of adding 24h: on 2026-03-29 the UK clocks jump from
// 01:00 to 02:00, so "tomorrow 08:00" is only 19 hours away, not 20.
func TestEvaluateAcrossDaylightSaving(t *testing.T) {
	s := mustSchedule(t, platformv1alpha1.ScalingScheduleSpec{
		TimeZone:        "Europe/London",
		DefaultReplicas: 1,
		Windows: []platformv1alpha1.ScheduleWindow{
			{Name: "morning", Start: "08:00", End: "09:00", Replicas: 3},
		},
	})

	now := time.Date(2026, time.March, 28, 12, 0, 0, 0, time.UTC) // Saturday, GMT (UTC+0)
	got := s.Evaluate(now)

	// Sunday 08:00 British Summer Time is 07:00 UTC.
	want := time.Date(2026, time.March, 29, 7, 0, 0, 0, time.UTC)
	if !got.NextTransition.Equal(want) {
		t.Fatalf("next transition = %v, want %v", got.NextTransition.UTC(), want)
	}
	if d := got.NextTransition.Sub(now); d != 19*time.Hour {
		t.Errorf("time until next transition = %v, want 19h", d)
	}
}

func TestFromSpecErrors(t *testing.T) {
	tests := []struct {
		name      string
		spec      platformv1alpha1.ScalingScheduleSpec
		wantField string
	}{
		{
			name: "unknown time zone",
			spec: platformv1alpha1.ScalingScheduleSpec{
				TimeZone: "Mars/Olympus_Mons",
				Windows:  []platformv1alpha1.ScheduleWindow{{Name: "a", Start: "08:00", End: "09:00"}},
			},
			wantField: "spec.timeZone",
		},
		{
			name: "hour out of range",
			spec: platformv1alpha1.ScalingScheduleSpec{
				Windows: []platformv1alpha1.ScheduleWindow{{Name: "a", Start: "25:00", End: "09:00"}},
			},
			wantField: "spec.windows[0].start",
		},
		{
			name: "start equals end",
			spec: platformv1alpha1.ScalingScheduleSpec{
				Windows: []platformv1alpha1.ScheduleWindow{
					{Name: "ok", Start: "01:00", End: "02:00"},
					{Name: "empty", Start: "09:00", End: "09:00"},
				},
			},
			wantField: "spec.windows[1].end",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := FromSpec(tc.spec)
			if len(errs) != 1 {
				t.Fatalf("got %d errors (%v), want exactly 1", len(errs), errs)
			}
			if errs[0].Field != tc.wantField {
				t.Errorf("error field = %q, want %q", errs[0].Field, tc.wantField)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	window := func(name string, days []platformv1alpha1.Weekday, start, end string) platformv1alpha1.ScheduleWindow {
		return platformv1alpha1.ScheduleWindow{Name: name, Days: days, Start: start, End: end}
	}
	weekdays := []platformv1alpha1.Weekday{"Mon", "Tue", "Wed", "Thu", "Fri"}

	tests := []struct {
		name    string
		windows []platformv1alpha1.ScheduleWindow
		want    int
	}{
		{
			name:    "disjoint windows",
			windows: []platformv1alpha1.ScheduleWindow{window("am", nil, "06:00", "12:00"), window("pm", nil, "12:00", "18:00")},
			want:    0,
		},
		{
			name:    "lunch peak inside business hours",
			windows: []platformv1alpha1.ScheduleWindow{window("business", weekdays, "08:00", "18:00"), window("lunch", nil, "12:00", "14:00")},
			want:    1,
		},
		{
			name: "saturday night wraps into sunday morning",
			windows: []platformv1alpha1.ScheduleWindow{
				window("sat-night", []platformv1alpha1.Weekday{"Sat"}, "22:00", "06:00"),
				window("sun-early", []platformv1alpha1.Weekday{"Sun"}, "05:00", "07:00"),
			},
			want: 1,
		},
		{
			name: "same hours on different days",
			windows: []platformv1alpha1.ScheduleWindow{
				window("mon", []platformv1alpha1.Weekday{"Mon"}, "08:00", "18:00"),
				window("tue", []platformv1alpha1.Weekday{"Tue"}, "08:00", "18:00"),
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSchedule(t, platformv1alpha1.ScalingScheduleSpec{Windows: tc.windows})
			if got := s.Overlaps(); len(got) != tc.want {
				t.Errorf("Overlaps() = %v, want %d overlap(s)", got, tc.want)
			}
		})
	}
}
