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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

// These tests call Reconcile() directly instead of running the controller
// in the manager. With a fake clock we decide what time it is, so a test
// can cover "Monday 09:00" and then "Monday 18:00" in milliseconds, and
// every assertion runs right after the reconcile without Eventually.
//
// The WebApp controller is still running in the background manager (see
// suite_test.go), so the WebApps created here get real Deployments too.
var _ = Describe("ScalingSchedule Controller", func() {
	var (
		london     *time.Location
		fakeClock  *clocktesting.FakeClock
		recorder   *events.FakeRecorder
		reconciler *ScalingScheduleReconciler
	)

	BeforeEach(func() {
		var err error
		london, err = time.LoadLocation("Europe/London")
		Expect(err).NotTo(HaveOccurred())

		// Monday 19 October 2026, 09:00 in London: inside business hours.
		fakeClock = clocktesting.NewFakeClock(time.Date(2026, time.October, 19, 9, 0, 0, 0, london))
		recorder = events.NewFakeRecorder(20)
		reconciler = &ScalingScheduleReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Recorder: recorder,
			Clock:    fakeClock,
		}
	})

	reconcileSchedule := func(name string) ctrl.Result {
		GinkgoHelper()
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	getSchedule := func(name string) *platformv1alpha1.ScalingSchedule {
		GinkgoHelper()
		ss := &platformv1alpha1.ScalingSchedule{}
		Expect(k8sClient.Get(ctx, key(name), ss)).To(Succeed())
		return ss
	}

	webAppReplicas := func(name string) int32 {
		GinkgoHelper()
		app := &platformv1alpha1.WebApp{}
		Expect(k8sClient.Get(ctx, key(name), app)).To(Succeed())
		return *app.Spec.Replicas
	}

	businessHours := func(name, target string) *platformv1alpha1.ScalingSchedule {
		return &platformv1alpha1.ScalingSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: platformv1alpha1.ScalingScheduleSpec{
				TargetRef:       platformv1alpha1.TargetReference{Name: target},
				TimeZone:        "Europe/London",
				DefaultReplicas: 2,
				Windows: []platformv1alpha1.ScheduleWindow{{
					Name:     "business",
					Days:     []platformv1alpha1.Weekday{"Mon", "Tue", "Wed", "Thu", "Fri"},
					Start:    "08:00",
					End:      "18:00",
					Replicas: 6,
				}},
			},
		}
	}

	It("scales the target in and out of a window", func() {
		Expect(k8sClient.Create(ctx, newWebApp("shop", func(a *platformv1alpha1.WebApp) {
			a.Spec.Replicas = new(int32(2))
		}))).To(Succeed())
		Expect(k8sClient.Create(ctx, businessHours("shop-hours", "shop"))).To(Succeed())

		By("evaluating at 09:00 on a Monday")
		result := reconcileSchedule("shop-hours")
		Expect(webAppReplicas("shop")).To(Equal(int32(6)))

		ss := getSchedule("shop-hours")
		Expect(ss.Status.ActiveWindow).To(Equal("business"))
		Expect(ss.Status.DesiredReplicas).To(HaveValue(Equal(int32(6))))
		Expect(ss.Status.NextTransitionTime.Time).To(BeTemporally("==", time.Date(2026, time.October, 19, 18, 0, 0, 0, london)))
		Expect(meta.IsStatusConditionTrue(ss.Status.Conditions, platformv1alpha1.ConditionReady)).To(BeTrue())
		Expect(recorder.Events).To(Receive(ContainSubstring("Scaled")))

		// The next boundary is 9h away, but the controller never sleeps longer
		// than maxRequeue. That is a safety net against bugs and clock jumps.
		Expect(result.RequeueAfter).To(Equal(maxRequeue))

		By("asking to be woken just after the boundary once it is near")
		fakeClock.SetTime(time.Date(2026, time.October, 19, 17, 30, 0, 0, london))
		result = reconcileSchedule("shop-hours")
		Expect(result.RequeueAfter).To(Equal(30*time.Minute + transitionSlack))

		By("re-asserting the schedule when someone scales the WebApp by hand")
		app := &platformv1alpha1.WebApp{}
		Expect(k8sClient.Get(ctx, key("shop"), app)).To(Succeed())
		patch := client.MergeFrom(app.DeepCopy())
		app.Spec.Replicas = new(int32(1))
		Expect(k8sClient.Patch(ctx, app, patch)).To(Succeed())

		reconcileSchedule("shop-hours")
		Expect(webAppReplicas("shop")).To(Equal(int32(6)))

		By("moving the clock past the end of the window")
		fakeClock.SetTime(time.Date(2026, time.October, 19, 18, 0, 1, 0, london))
		reconcileSchedule("shop-hours")
		Expect(webAppReplicas("shop")).To(Equal(int32(2)))

		ss = getSchedule("shop-hours")
		Expect(ss.Status.ActiveWindow).To(BeEmpty())
		Expect(ss.Status.DesiredReplicas).To(HaveValue(Equal(int32(2))))
		Expect(ss.Status.NextTransitionTime.Time).To(BeTemporally("==", time.Date(2026, time.October, 20, 8, 0, 0, 0, london)))
	})

	It("waits for a target that does not exist yet", func() {
		Expect(k8sClient.Create(ctx, businessHours("orphan-hours", "not-created-yet"))).To(Succeed())

		result := reconcileSchedule("orphan-hours")

		cond := meta.FindStatusCondition(getSchedule("orphan-hours").Status.Conditions, platformv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(platformv1alpha1.ReasonTargetNotFound))
		// Not an error (nothing to retry), but it still wakes up at the
		// next transition in case the WebApp watch was somehow missed.
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
	})

	It("leaves the target alone while suspended", func() {
		Expect(k8sClient.Create(ctx, newWebApp("quiet-shop", func(a *platformv1alpha1.WebApp) {
			a.Spec.Replicas = new(int32(3))
		}))).To(Succeed())
		ss := businessHours("quiet-hours", "quiet-shop")
		ss.Spec.Suspend = true
		Expect(k8sClient.Create(ctx, ss)).To(Succeed())

		result := reconcileSchedule("quiet-hours")

		Expect(webAppReplicas("quiet-shop")).To(Equal(int32(3)))
		Expect(result.RequeueAfter).To(BeZero())
		cond := meta.FindStatusCondition(getSchedule("quiet-hours").Status.Conditions, platformv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(platformv1alpha1.ReasonSuspended))
	})

	It("is protected by CEL: targetRef cannot change after creation", func() {
		Expect(k8sClient.Create(ctx, businessHours("fixed-target", "shop"))).To(Succeed())

		ss := getSchedule("fixed-target")
		ss.Spec.TargetRef.Name = "another-app"
		err := k8sClient.Update(ctx, ss)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("targetRef is immutable"))
	})
})
