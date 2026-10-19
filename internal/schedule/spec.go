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

import (
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/validation/field"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

var weekdays = map[platformv1alpha1.Weekday]time.Weekday{
	"Sun": time.Sunday,
	"Mon": time.Monday,
	"Tue": time.Tuesday,
	"Wed": time.Wednesday,
	"Thu": time.Thursday,
	"Fri": time.Friday,
	"Sat": time.Saturday,
}

// FromSpec converts the user-facing API type into a Schedule.
//
// The CRD schema already checks most fields, but some checks need Go: the
// API server cannot tell whether "Mars/Olympus_Mons" is a real time zone.
// Both the validating webhook and the controller call this function, so they
// can never disagree about what "valid" means.
//
// Errors come back as a field.ErrorList, the Kubernetes way of saying
// "spec.windows[2].start: Invalid value: ...". The webhook passes this list
// straight to the API server, so users see precise messages.
func FromSpec(spec platformv1alpha1.ScalingScheduleSpec) (*Schedule, field.ErrorList) {
	var errs field.ErrorList
	specPath := field.NewPath("spec")

	tz := spec.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		errs = append(errs, field.Invalid(specPath.Child("timeZone"), spec.TimeZone, "unknown IANA time zone"))
	}

	s := &Schedule{
		Location:        loc,
		DefaultReplicas: spec.DefaultReplicas,
		Windows:         make([]Window, 0, len(spec.Windows)),
	}

	for i, w := range spec.Windows {
		wPath := specPath.Child("windows").Index(i)
		win := Window{Name: w.Name, Replicas: w.Replicas}

		var startErr, endErr error
		win.Start, startErr = ParseClock(w.Start)
		if startErr != nil {
			errs = append(errs, field.Invalid(wPath.Child("start"), w.Start, startErr.Error()))
		}
		win.End, endErr = ParseClock(w.End)
		if endErr != nil {
			errs = append(errs, field.Invalid(wPath.Child("end"), w.End, endErr.Error()))
		}
		if startErr == nil && endErr == nil && win.Start == win.End {
			errs = append(errs, field.Invalid(wPath.Child("end"), w.End, "must differ from start"))
		}

		for j, d := range w.Days {
			wd, ok := weekdays[d]
			if !ok {
				errs = append(errs, field.NotSupported(wPath.Child("days").Index(j), d, []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}))
				continue
			}
			win.Days = append(win.Days, wd)
		}
		s.Windows = append(s.Windows, win)
	}

	if len(errs) > 0 {
		return nil, errs
	}
	return s, nil
}

// ParseClock converts a 24h "HH:MM" string into minutes after midnight.
func ParseClock(v string) (int, error) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, errors.New("want 24h HH:MM, e.g. 08:30")
	}
	return t.Hour()*60 + t.Minute(), nil
}
