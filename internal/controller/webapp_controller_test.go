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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
	"github.com/example/platform-operator/internal/catalog"
)

const testNamespace = "default"

// newWebApp builds a minimal WebApp. The variadic "option functions" let
// each test tweak only what it cares about, a common Go pattern.
func newWebApp(name string, opts ...func(*platformv1alpha1.WebApp)) *platformv1alpha1.WebApp {
	app := &platformv1alpha1.WebApp{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       platformv1alpha1.WebAppSpec{Image: "nginx:1.27"},
	}
	for _, opt := range opts {
		opt(app)
	}
	return app
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: testNamespace, Name: name}
}

// conditionOf fetches a WebApp and returns one of its conditions (or nil).
// The Gomega argument lets assertions inside fail just this Eventually
// attempt instead of the whole test.
func conditionOf(g Gomega, name, condType string) *metav1.Condition {
	app := &platformv1alpha1.WebApp{}
	g.Expect(k8sClient.Get(ctx, key(name), app)).To(Succeed())
	return meta.FindStatusCondition(app.Status.Conditions, condType)
}

func catalogData(g Gomega) map[string]string {
	cm := &corev1.ConfigMap{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: catalogNamespace, Name: catalogName}, cm)).To(Succeed())
	return cm.Data
}

func configHashOf(g Gomega, name string) string {
	dep := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, key(name), dep)).To(Succeed())
	return dep.Spec.Template.Annotations[configHashAnnotation]
}

// updateWebApp does a read-modify-write, retried by Eventually if it hits
// a conflict because the controller wrote the object in between.
func updateWebApp(name string, mutate func(*platformv1alpha1.WebApp)) {
	GinkgoHelper()
	Eventually(func() error {
		app := &platformv1alpha1.WebApp{}
		if err := k8sClient.Get(ctx, key(name), app); err != nil {
			return err
		}
		mutate(app)
		return k8sClient.Update(ctx, app)
	}).Should(Succeed())
}

var _ = Describe("WebApp Controller", func() {
	It("creates a ConfigMap, Deployment and Service owned by the WebApp", func() {
		app := newWebApp("hello", func(a *platformv1alpha1.WebApp) {
			a.Spec.Config = map[string]string{"GREETING": "hello"}
		})
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		By("adding the finalizer")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key("hello"), app)).To(Succeed())
			g.Expect(app.Finalizers).To(ContainElement(webAppFinalizer))
		}).Should(Succeed())

		By("creating a Deployment with CRD defaults applied")
		dep := &appsv1.Deployment{}
		Eventually(func() error { return k8sClient.Get(ctx, key("hello"), dep) }).Should(Succeed())
		Expect(metav1.IsControlledBy(dep, app)).To(BeTrue(), "Deployment should have a controller owner reference to the WebApp")
		Expect(dep.Spec.Replicas).To(HaveValue(Equal(int32(1))), "replicas defaults to 1 via +kubebuilder:default")
		container := dep.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal("nginx:1.27"))
		Expect(container.Env).To(ContainElement(corev1.EnvVar{Name: "PORT", Value: "8080"}))
		Expect(dep.Spec.Template.Annotations).To(HaveKey(configHashAnnotation))

		By("rendering spec.config into a ConfigMap")
		cm := &corev1.ConfigMap{}
		Eventually(func() error { return k8sClient.Get(ctx, key("hello-config"), cm) }).Should(Succeed())
		Expect(cm.Data).To(Equal(map[string]string{"GREETING": "hello"}))
		Expect(metav1.IsControlledBy(cm, app)).To(BeTrue())

		By("exposing it with a Service on port 80")
		svc := &corev1.Service{}
		Eventually(func() error { return k8sClient.Get(ctx, key("hello"), svc) }).Should(Succeed())
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(80)))
		Expect(svc.Spec.Selector).To(Equal(selectorLabels(app)))

		By("registering it in the cross-namespace catalog")
		Eventually(func(g Gomega) {
			g.Expect(catalogData(g)).To(HaveKeyWithValue(catalog.Key(testNamespace, "hello"), ContainSubstring(`"image":"nginx:1.27"`)))
		}).Should(Succeed())

		By("reporting status")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key("hello"), app)).To(Succeed())
			g.Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
			g.Expect(app.Status.URL).To(Equal("http://hello.default.svc.cluster.local"))
			// envtest has no kube-controller-manager, so no pods ever start.
			g.Expect(meta.IsStatusConditionTrue(app.Status.Conditions, platformv1alpha1.ConditionProgressing)).To(BeTrue())
			g.Expect(meta.IsStatusConditionFalse(app.Status.Conditions, platformv1alpha1.ConditionAvailable)).To(BeTrue())
		}).Should(Succeed())
	})

	It("reports Available once the Deployment's pods are ready", func() {
		Expect(k8sClient.Create(ctx, newWebApp("ready-app", func(a *platformv1alpha1.WebApp) {
			a.Spec.Replicas = new(int32(2))
		}))).To(Succeed())

		By("pretending to be the Deployment controller")
		// In a real cluster kube-controller-manager writes this status as the
		// pods come up. Here we write it ourselves.
		Eventually(func() error {
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, key("ready-app"), dep); err != nil {
				return err
			}
			dep.Status = appsv1.DeploymentStatus{
				ObservedGeneration: dep.Generation,
				Replicas:           2,
				UpdatedReplicas:    2,
				ReadyReplicas:      2,
				AvailableReplicas:  2,
				Conditions: []appsv1.DeploymentCondition{{
					Type:   appsv1.DeploymentAvailable,
					Status: corev1.ConditionTrue,
					Reason: "MinimumReplicasAvailable",
				}},
			}
			return k8sClient.Status().Update(ctx, dep)
		}).Should(Succeed())

		By("watching the owned Deployment and updating the WebApp's status")
		Eventually(func(g Gomega) {
			app := &platformv1alpha1.WebApp{}
			g.Expect(k8sClient.Get(ctx, key("ready-app"), app)).To(Succeed())
			g.Expect(app.Status.ReadyReplicas).To(Equal(int32(2)))
			g.Expect(meta.IsStatusConditionTrue(app.Status.Conditions, platformv1alpha1.ConditionAvailable)).To(BeTrue())
			g.Expect(meta.IsStatusConditionFalse(app.Status.Conditions, platformv1alpha1.ConditionProgressing)).To(BeTrue())
		}).Should(Succeed())
	})

	It("does not rewrite its children when nothing changed", func() {
		// This guards against the classic operator bug: a mutate function
		// that fights the API server's defaulting and sends an update on
		// every single reconcile.
		Expect(k8sClient.Create(ctx, newWebApp("steady"))).To(Succeed())

		var resourceVersion string
		Eventually(func(g Gomega) {
			app := &platformv1alpha1.WebApp{}
			g.Expect(k8sClient.Get(ctx, key("steady"), app)).To(Succeed())
			g.Expect(app.Status.ObservedGeneration).To(Equal(app.Generation))
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key("steady"), dep)).To(Succeed())
			resourceVersion = dep.ResourceVersion
		}).Should(Succeed())

		By("triggering more reconciles with an unrelated annotation change")
		updateWebApp("steady", func(a *platformv1alpha1.WebApp) {
			a.Annotations = map[string]string{"example.com/touched": time.Now().Format(time.RFC3339Nano)}
		})

		Consistently(func(g Gomega) string {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key("steady"), dep)).To(Succeed())
			return dep.ResourceVersion
		}, 2*time.Second).Should(Equal(resourceVersion))
	})

	It("rolls the pods when spec.config changes", func() {
		Expect(k8sClient.Create(ctx, newWebApp("rolling", func(a *platformv1alpha1.WebApp) {
			a.Spec.Config = map[string]string{"LOG_LEVEL": "info"}
		}))).To(Succeed())

		var before string
		Eventually(func(g Gomega) {
			before = configHashOf(g, "rolling")
			g.Expect(before).NotTo(BeEmpty())
		}).Should(Succeed())

		updateWebApp("rolling", func(a *platformv1alpha1.WebApp) { a.Spec.Config["LOG_LEVEL"] = "debug" })

		Eventually(func(g Gomega) {
			after := configHashOf(g, "rolling")
			g.Expect(after).NotTo(Equal(before), "a new pod-template annotation makes the Deployment roll")

			app := &platformv1alpha1.WebApp{}
			g.Expect(k8sClient.Get(ctx, key("rolling"), app)).To(Succeed())
			g.Expect(app.Status.ConfigHash).To(Equal(after))
		}).Should(Succeed())
	})

	It("waits for a missing Secret, then rolls the pods when the Secret changes", func() {
		Expect(k8sClient.Create(ctx, newWebApp("with-secret", func(a *platformv1alpha1.WebApp) {
			a.Spec.SecretRef = &corev1.LocalObjectReference{Name: "with-secret-creds"}
		}))).To(Succeed())

		By("reporting Degraded while the Secret is missing")
		Eventually(func(g Gomega) {
			cond := conditionOf(g, "with-secret", platformv1alpha1.ConditionDegraded)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal(platformv1alpha1.ReasonSecretNotFound))
		}).Should(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("with-secret"), &appsv1.Deployment{}))).To(BeTrue())

		By("creating the Secret, which the metadata-only watch picks up")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "with-secret-creds", Namespace: testNamespace},
			StringData: map[string]string{"DB_PASSWORD": "s3cr3t"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		var before string
		Eventually(func(g Gomega) {
			before = configHashOf(g, "with-secret")
			g.Expect(before).NotTo(BeEmpty())
		}).Should(Succeed())

		By("rotating the Secret")
		secret.StringData = map[string]string{"DB_PASSWORD": "rotated"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		Eventually(func(g Gomega) string { return configHashOf(g, "with-secret") }).ShouldNot(Equal(before))
	})

	It("creates the Ingress when spec.ingress is set and deletes it when removed", func() {
		Expect(k8sClient.Create(ctx, newWebApp("public", func(a *platformv1alpha1.WebApp) {
			a.Spec.Ingress = &platformv1alpha1.IngressSpec{Host: "public.example.com"}
		}))).To(Succeed())

		ing := &networkingv1.Ingress{}
		Eventually(func() error { return k8sClient.Get(ctx, key("public"), ing) }).Should(Succeed())
		Expect(ing.Spec.Rules[0].Host).To(Equal("public.example.com"))
		Expect(ing.Spec.Rules[0].HTTP.Paths[0].Path).To(Equal("/"), "path defaults to / via +kubebuilder:default")

		Eventually(func(g Gomega) {
			app := &platformv1alpha1.WebApp{}
			g.Expect(k8sClient.Get(ctx, key("public"), app)).To(Succeed())
			g.Expect(app.Status.URL).To(Equal("http://public.example.com/"))
		}).Should(Succeed())

		updateWebApp("public", func(a *platformv1alpha1.WebApp) { a.Spec.Ingress = nil })

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key("public"), &networkingv1.Ingress{}))
		}).Should(BeTrue())
	})

	It("stops touching children while paused", func() {
		Expect(k8sClient.Create(ctx, newWebApp("frozen"))).To(Succeed())
		Eventually(func() error { return k8sClient.Get(ctx, key("frozen"), &appsv1.Deployment{}) }).Should(Succeed())

		updateWebApp("frozen", func(a *platformv1alpha1.WebApp) {
			a.Spec.Paused = true
			a.Spec.Image = "nginx:1.28"
		})
		Eventually(func(g Gomega) {
			cond := conditionOf(g, "frozen", platformv1alpha1.ConditionProgressing)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(platformv1alpha1.ReasonPaused))
		}).Should(Succeed())

		imageOf := func(g Gomega) string {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, key("frozen"), dep)).To(Succeed())
			return dep.Spec.Template.Spec.Containers[0].Image
		}
		Consistently(imageOf, time.Second).Should(Equal("nginx:1.27"))

		updateWebApp("frozen", func(a *platformv1alpha1.WebApp) { a.Spec.Paused = false })
		Eventually(imageOf).Should(Equal("nginx:1.28"))
	})

	It("refuses to take over a Deployment it did not create", func() {
		labels := map[string]string{"app": "squatter"}
		existing := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "squatter", Namespace: testNamespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1.36"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())
		Expect(k8sClient.Create(ctx, newWebApp("squatter"))).To(Succeed())

		Eventually(func(g Gomega) {
			cond := conditionOf(g, "squatter", platformv1alpha1.ConditionDegraded)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Message).To(ContainSubstring("not managed by WebApp"))
		}).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(existing), existing)).To(Succeed())
		Expect(existing.OwnerReferences).To(BeEmpty())
		Expect(existing.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox:1.36"))
	})

	It("removes its catalog entry before the WebApp is deleted", func() {
		app := newWebApp("short-lived")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		entryKey := catalog.Key(testNamespace, "short-lived")
		Eventually(func(g Gomega) { g.Expect(catalogData(g)).To(HaveKey(entryKey)) }).Should(Succeed())

		Expect(k8sClient.Delete(ctx, app)).To(Succeed())

		By("running the finalizer, after which the API server removes the object")
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key("short-lived"), &platformv1alpha1.WebApp{}))
		}).Should(BeTrue())
		Eventually(func(g Gomega) { g.Expect(catalogData(g)).NotTo(HaveKey(entryKey)) }).Should(Succeed())
	})

	It("is rejected by the API server's CEL rules when it sets PORT itself", func() {
		// No controller or webhook involved: the CRD's x-kubernetes-validations
		// rule runs inside the API server.
		err := k8sClient.Create(ctx, newWebApp("bad-port", func(a *platformv1alpha1.WebApp) {
			a.Spec.Env = []corev1.EnvVar{{Name: "PORT", Value: "9999"}}
		}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected 422 Invalid, got %v", err)
		Expect(err.Error()).To(ContainSubstring("env var PORT is reserved"))
	})
})
