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
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Sample manifests are documentation, and stale documentation is worse than
// none. This spec submits every file in config/samples to the API server, so
// a schema change that breaks a sample fails CI.
var _ = Describe("Sample manifests", func() {
	It("are accepted by the API server", func() {
		const ns = "samples"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())

		for _, f := range files {
			if filepath.Base(f) == "kustomization.yaml" {
				continue
			}
			By("creating " + filepath.Base(f))
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())

			// Unstructured holds any object as a map, without a Go type. It's
			// handy for generic tooling like this.
			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(data, &obj.Object)).To(Succeed())
			obj.SetNamespace(ns)
			Expect(k8sClient.Create(ctx, obj)).To(Succeed(), "sample %s was rejected", f)
		}
	})
})
