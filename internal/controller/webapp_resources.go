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

// This file holds the "desired state" builders for a WebApp's children.
//
// Each mutateX function takes an object that may be brand new (empty) or may
// have just been read from the cluster, and writes the fields the operator
// owns onto it. The functions are pure: no API calls and no clock, so
// webapp_resources_test.go can test them with plain `go test`.
//
// They only touch fields we own and leave the rest alone. The API server
// fills in defaults (imagePullPolicy, terminationMessagePath, clusterIP, ...)
// after we create an object. Replacing whole structs on every reconcile would
// erase those defaults, the object would never match what is stored, and we
// would send a pointless update on every pass. See docs/04-webapp-controller.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	platformv1alpha1 "github.com/example/platform-operator/api/v1alpha1"
)

const (
	// appContainerName is the name of the WebApp's container in the pod.
	appContainerName = "app"
	// httpPortName names the container port. The Service and readiness probe
	// refer to the port by name, so changing spec.port needs no other edits.
	httpPortName = "http"
	// servicePort is the port the Service listens on inside the cluster.
	servicePort = 80
	// configHashAnnotation is set on the pod template. Changing its value
	// changes the template, which makes the Deployment start a rolling update.
	configHashAnnotation = "platform.example.com/config-hash"
	managedByValue       = "platform-operator"
)

// configMapName is the name of the ConfigMap rendered from spec.config.
// The Deployment, Service and Ingress share the WebApp's own name.
func configMapName(app *platformv1alpha1.WebApp) string {
	return app.Name + "-config"
}

// selectorLabels identify the pods of one WebApp. Deployment selectors are
// immutable, so this set must never change once objects exist.
func selectorLabels(app *platformv1alpha1.WebApp) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "webapp",
		"app.kubernetes.io/instance": app.Name,
	}
}

// commonLabels go on every child object.
func commonLabels(app *platformv1alpha1.WebApp) map[string]string {
	l := selectorLabels(app)
	l["app.kubernetes.io/managed-by"] = managedByValue
	return l
}

// mergeLabels adds our labels to obj without removing labels others set.
func mergeLabels(obj metav1.Object, labels map[string]string) {
	merged := obj.GetLabels()
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, labels)
	obj.SetLabels(merged)
}

// desiredReplicas reads spec.replicas. The CRD defaults it to 1, but unit
// tests build objects without the API server, so handle nil here too.
func desiredReplicas(app *platformv1alpha1.WebApp) int32 {
	if app.Spec.Replicas == nil {
		return 1
	}
	return *app.Spec.Replicas
}

// computeConfigHash returns a short, stable fingerprint of everything that
// should restart the pods when it changes.
//
// Go randomises map iteration order on purpose, so we sort the keys first.
// Without sorting, the hash would change between runs and restart the pods
// for no reason.
func computeConfigHash(config map[string]string, secretVersion string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(config)) {
		_, _ = fmt.Fprintf(h, "%q=%q\n", k, config[k])
	}
	_, _ = fmt.Fprintf(h, "secret=%q\n", secretVersion)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// appURL is where the WebApp can be reached.
func appURL(app *platformv1alpha1.WebApp) string {
	if ing := app.Spec.Ingress; ing != nil {
		return "http://" + ing.Host + ingressPath(ing)
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", app.Name, app.Namespace)
}

func ingressPath(ing *platformv1alpha1.IngressSpec) string {
	if ing.Path == "" {
		return "/"
	}
	return ing.Path
}

func mutateConfigMap(cm *corev1.ConfigMap, app *platformv1alpha1.WebApp) {
	mergeLabels(cm, commonLabels(app))
	// The operator owns this ConfigMap's data outright, so replacing the
	// whole map is correct here. Keys deleted from spec.config disappear.
	cm.Data = maps.Clone(app.Spec.Config)
}

func mutateDeployment(dep *appsv1.Deployment, app *platformv1alpha1.WebApp, configHash string) {
	mergeLabels(dep, commonLabels(app))

	// spec.selector is immutable once the Deployment exists. Only set it on
	// create, which we detect by the object not having a resourceVersion yet.
	if dep.ResourceVersion == "" {
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app)}
	}
	// Since Go 1.26, new() also accepts a value: new(x) allocates a copy of x
	// and returns a pointer to it. Older code uses k8s.io/utils/ptr.To(x).
	dep.Spec.Replicas = new(desiredReplicas(app))

	tmpl := &dep.Spec.Template
	mergeLabels(&tmpl.ObjectMeta, commonLabels(app))
	if tmpl.Annotations == nil {
		tmpl.Annotations = map[string]string{}
	}
	tmpl.Annotations[configHashAnnotation] = configHash

	c := appContainer(&tmpl.Spec)
	c.Image = app.Spec.Image
	c.Ports = []corev1.ContainerPort{{
		Name:          httpPortName,
		ContainerPort: app.Spec.Port,
		Protocol:      corev1.ProtocolTCP,
	}}
	// PORT comes first so the app always gets it. The CRD's CEL rule stops
	// users from setting PORT themselves.
	c.Env = append([]corev1.EnvVar{{Name: "PORT", Value: strconv.Itoa(int(app.Spec.Port))}}, app.Spec.Env...)
	c.EnvFrom = envFromSources(app)
	c.Resources = app.Spec.Resources
	// Set every probe field explicitly, including the values the API server
	// would default. Otherwise we would "remove" the defaults on each pass
	// and keep sending updates (see the comment at the top of this file).
	c.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(httpPortName)},
		},
		PeriodSeconds:    10,
		TimeoutSeconds:   1,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}
}

// appContainer returns a pointer to the app container in spec, adding it if
// it is missing.
//
// Note `for i := range` plus &spec.Containers[i]. Writing
// `for _, c := range spec.Containers` would give us a copy of each
// container, and changes to the copy would be lost.
func appContainer(spec *corev1.PodSpec) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == appContainerName {
			return &spec.Containers[i]
		}
	}
	spec.Containers = append(spec.Containers, corev1.Container{Name: appContainerName})
	return &spec.Containers[len(spec.Containers)-1]
}

func envFromSources(app *platformv1alpha1.WebApp) []corev1.EnvFromSource {
	sources := []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(app)},
		},
	}}
	if app.Spec.SecretRef != nil {
		sources = append(sources, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *app.Spec.SecretRef},
		})
	}
	return sources
}

func mutateService(svc *corev1.Service, app *platformv1alpha1.WebApp) {
	mergeLabels(svc, commonLabels(app))
	svc.Spec.Selector = selectorLabels(app)
	svc.Spec.Ports = []corev1.ServicePort{{
		Name:       httpPortName,
		Port:       servicePort,
		TargetPort: intstr.FromString(httpPortName),
		Protocol:   corev1.ProtocolTCP,
	}}
}

func mutateIngress(ing *networkingv1.Ingress, app *platformv1alpha1.WebApp) {
	mergeLabels(ing, commonLabels(app))
	spec := app.Spec.Ingress
	ing.Spec.IngressClassName = spec.ClassName
	ing.Spec.Rules = []networkingv1.IngressRule{{
		Host: spec.Host,
		IngressRuleValue: networkingv1.IngressRuleValue{
			HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path:     ingressPath(spec),
					PathType: new(networkingv1.PathTypePrefix),
					Backend: networkingv1.IngressBackend{
						Service: &networkingv1.IngressServiceBackend{
							Name: app.Name,
							Port: networkingv1.ServiceBackendPort{Name: httpPortName},
						},
					},
				}},
			},
		},
	}}
}
