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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

var _ = Describe("WebApp Webhook", func() {
	var (
		obj       *platformv1alpha1.WebApp
		validator WebAppValidator
		defaulter WebAppDefaulter
	)

	BeforeEach(func() {
		obj = &platformv1alpha1.WebApp{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec:       platformv1alpha1.WebAppSpec{Image: "nginx:1.27"},
		}
		validator = WebAppValidator{}
		defaulter = WebAppDefaulter{}
	})

	// The first two Contexts call the Go methods directly: fast and precise.
	Context("When creating WebApp under Defaulting Webhook", func() {
		It("fills in resource requests when neither requests nor limits are set", func() {
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Resources.Requests.Cpu().String()).To(Equal("50m"))
			Expect(obj.Spec.Resources.Requests.Memory().String()).To(Equal("64Mi"))
		})

		It("leaves resources alone when the user set limits", func() {
			obj.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m")}
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec.Resources.Requests).To(BeEmpty(), "Kubernetes defaults requests to limits; adding 50m would exceed the 20m limit")
		})
	})

	Context("When creating or updating WebApp under Validating Webhook", func() {
		It("accepts a pinned image without warnings", func() {
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())
		})

		It("warns, but does not reject, an unpinned image", func() {
			obj.Spec.Image = "nginx:latest"
			warnings, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(ContainElement(ContainSubstring("Pin a version")))
		})

		It("rejects requests that exceed limits", func() {
			obj.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			}
			_, err := validator.ValidateUpdate(ctx, obj.DeepCopy(), obj)
			Expect(err).To(MatchError(ContainSubstring("spec.resources.requests[memory]")))
		})
	})

	// DescribeTable is Ginkgo's version of a table-driven test.
	DescribeTable("imageIsPinned",
		func(image string, want bool) {
			Expect(imageIsPinned(image)).To(Equal(want))
		},
		Entry("tagged", "nginx:1.27", true),
		Entry("no tag", "nginx", false),
		Entry("latest tag", "nginx:latest", false),
		Entry("registry port and tag", "localhost:5000/team/app:v2", true),
		Entry("registry port, no tag", "localhost:5000/team/app", false),
		Entry("digest", "nginx@sha256:0d17b565c37bcbd895e9d92315a05c1c3c9a29f762b011a10c54a66cd53c9b31", true),
	)

	// These go through the real API server, which calls our webhook server
	// over HTTPS, just like a cluster would.
	Context("When submitted through the API server", func() {
		It("runs the mutating webhook before storing the object", func() {
			app := obj.DeepCopy()
			app.Name = "defaulted-by-webhook"
			Expect(k8sClient.Create(ctx, app)).To(Succeed())
			// Create updates `app` in place with what the server stored.
			Expect(app.Spec.Resources.Requests.Cpu().String()).To(Equal("50m"))
		})

		It("returns validation errors to the client as 422 Invalid", func() {
			app := obj.DeepCopy()
			app.Name = "too-greedy"
			app.Spec.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
			}
			err := k8sClient.Create(ctx, app)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("must be less than or equal to the cpu limit"))
		})
	})
})
