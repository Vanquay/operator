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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

var _ = Describe("ScalingSchedule Webhook", func() {
	var validator ScalingScheduleValidator

	newSchedule := func(name, target string) *platformv1alpha1.ScalingSchedule {
		return &platformv1alpha1.ScalingSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: platformv1alpha1.ScalingScheduleSpec{
				TargetRef:       platformv1alpha1.TargetReference{Name: target},
				TimeZone:        "America/New_York",
				DefaultReplicas: 1,
				Windows: []platformv1alpha1.ScheduleWindow{
					{Name: "business", Start: "09:00", End: "17:00", Replicas: 4},
				},
			},
		}
	}

	BeforeEach(func() {
		// The validator only needs a client.Reader, and the test client is
		// one. That's the payoff of depending on a small interface.
		validator = ScalingScheduleValidator{Client: k8sClient}
	})

	Context("When creating or updating ScalingSchedule under Validating Webhook", func() {
		It("rejects an unknown time zone with a precise field path", func() {
			ss := newSchedule("bad-tz", "app")
			ss.Spec.TimeZone = "Mars/Olympus_Mons"
			_, err := validator.ValidateCreate(ctx, ss)
			Expect(err).To(MatchError(ContainSubstring("spec.timeZone")))
		})

		It("warns about overlapping windows and a target that doesn't exist yet", func() {
			ss := newSchedule("warn-me", "not-there")
			ss.Spec.Windows = append(ss.Spec.Windows,
				platformv1alpha1.ScheduleWindow{Name: "lunch-rush", Start: "12:00", End: "14:00", Replicas: 8})

			warnings, err := validator.ValidateCreate(ctx, ss)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring(`"business" and "lunch-rush" overlap`)))
			Expect(warnings).To(ContainElement(ContainSubstring("does not exist yet")))
		})
	})

	Context("When submitted through the API server", func() {
		It("allows only one schedule per WebApp", func() {
			Expect(k8sClient.Create(ctx, newSchedule("checkout-day", "checkout"))).To(Succeed())

			err := k8sClient.Create(ctx, newSchedule("checkout-night", "checkout"))
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(`already targeted by ScalingSchedule "checkout-day"`))
		})
	})
})
