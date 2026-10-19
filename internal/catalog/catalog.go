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

// Package catalog keeps a cluster-wide directory of every WebApp.
//
// The directory is a single ConfigMap in the operator's namespace, so a
// dashboard or CLI can discover all apps with one read:
//
//	kubectl get configmap webapp-catalog -n platform-operator-system -o yaml
//
// It exists to show why operators need finalizers. Kubernetes garbage
// collection follows owner references, and an owner reference cannot point
// across namespaces. The catalog lives in a different namespace from the
// WebApps, so nothing would remove an entry when its WebApp is deleted. The
// WebApp controller adds a finalizer and calls Deregister itself.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Entry is one WebApp's record in the catalog.
type Entry struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	URL       string `json:"url"`
}

// Catalog is what the WebApp controller depends on.
//
// Go interfaces are satisfied implicitly: any type with these two methods is
// a Catalog. No "implements" keyword is needed. The controller only knows
// about this interface, so tests or a future HTTP-backed registry can swap in
// a different implementation without touching the controller.
type Catalog interface {
	Register(ctx context.Context, e Entry) error
	Deregister(ctx context.Context, namespace, name string) error
}

// ConfigMapCatalog stores entries in a ConfigMap, one key per WebApp.
type ConfigMapCatalog struct {
	// Reader should be an uncached reader (mgr.GetAPIReader()). The catalog is
	// a read-modify-write on a single hot object, and reading from a possibly
	// stale cache would cause needless update conflicts.
	Reader client.Reader
	Writer client.Writer

	Namespace string
	Name      string
}

// This line fails to compile if *ConfigMapCatalog ever stops satisfying
// Catalog. It is a common Go idiom for checking an interface at compile time.
var _ Catalog = (*ConfigMapCatalog)(nil)

// Key returns the ConfigMap key for a WebApp. Namespace names cannot contain
// dots, so "namespace.name" is unambiguous.
func Key(namespace, name string) string {
	return namespace + "." + name
}

// Register adds or refreshes the entry for a WebApp.
func (c *ConfigMapCatalog) Register(ctx context.Context, e Entry) error {
	value, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding catalog entry: %w", err)
	}
	key := Key(e.Namespace, e.Name)
	return c.mutate(ctx, func(data map[string]string) bool {
		if data[key] == string(value) {
			return false
		}
		data[key] = string(value)
		return true
	})
}

// Deregister removes a WebApp's entry. Removing a missing entry is not an
// error: a finalizer must be safe to run more than once.
func (c *ConfigMapCatalog) Deregister(ctx context.Context, namespace, name string) error {
	key := Key(namespace, name)
	return c.mutate(ctx, func(data map[string]string) bool {
		if _, ok := data[key]; !ok {
			return false
		}
		delete(data, key)
		return true
	})
}

// mutate reads the ConfigMap, lets fn change its data, and writes it back.
//
// Several WebApps can be reconciled at the same time, so two goroutines may
// update the ConfigMap at once. The API server uses optimistic concurrency:
// every object has a resourceVersion, and an update based on an old version
// fails with 409 Conflict. retry.OnError re-runs the whole read-modify-write
// when that happens.
func (c *ConfigMapCatalog) mutate(ctx context.Context, fn func(data map[string]string) (changed bool)) error {
	retriable := func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}
	return retry.OnError(retry.DefaultRetry, retriable, func() error {
		cm := &corev1.ConfigMap{}
		err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, cm)
		switch {
		case apierrors.IsNotFound(err):
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: c.Namespace,
					Name:      c.Name,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "platform-operator"},
				},
				Data: map[string]string{},
			}
			if !fn(cm.Data) {
				return nil
			}
			return c.Writer.Create(ctx, cm)
		case err != nil:
			return err
		}

		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if !fn(cm.Data) {
			return nil
		}
		return c.Writer.Update(ctx, cm)
	})
}
