# 8. Running locally and deploying

## Prerequisites

| Tool | Install | Notes |
|---|---|---|
| Go 1.26+ | already installed | `go version` |
| kubebuilder | `brew install kubebuilder` | Only needed to scaffold new APIs/webhooks |
| Docker runtime | Docker Desktop, OrbStack, or `brew install colima && colima start` | kind runs Kubernetes nodes as containers |
| kind | `brew install kind` | Local clusters |
| kubectl | `brew install kubectl` | **Upgrade it.** kubectl supports only ±1 minor version of the cluster, and v1.25 is far too old for 1.37. |

Check that Docker is running: `docker info` should print server details, not "Cannot connect".

## Option A: run the operator on your laptop (fastest edit loop)

The operator runs as a normal Go process using your kubeconfig. Restart it after every code
change; no images are involved.

```sh
kind create cluster --name platform-dev
make install                      # CRDs only
ENABLE_WEBHOOKS=false make run    # operator in the foreground; Ctrl-C to stop
```

In a second terminal:

```sh
make dev-samples
kubectl get webapps,scalingschedules
```

With `make run`:
- webhooks are off (see [06-webhooks.md](06-webhooks.md#running-without-webhooks)), but CRD
  validation and CEL still apply,
- the catalog ConfigMap goes into `default` (there is no `POD_NAMESPACE`),
- you run with *your* kubeconfig permissions (probably cluster-admin), so RBAC mistakes don't
  show up.

To see the custom metrics locally:

```sh
go run ./cmd/main.go --metrics-bind-address=:8080 --metrics-secure=false   # with ENABLE_WEBHOOKS=false
curl -s localhost:8080/metrics | grep -E '^(platform_|controller_runtime_reconcile_total)'
```

## Option B: deploy into the cluster (production-like)

```sh
make dev-cluster     # kind cluster "platform-dev" + cert-manager
make dev-deploy      # docker build → kind load → kustomize deploy → wait for rollout
make dev-samples
```

This runs exactly what production would: the distroless image, the generated RBAC, webhooks with
cert-manager certificates, leader election, and secured metrics.

```sh
kubectl get pods -n platform-operator-system
kubectl logs -n platform-operator-system deploy/platform-operator-controller-manager -f
```

After changing code, run `make dev-deploy` again. It rebuilds the image and restarts the
Deployment.

To read the metrics in-cluster (HTTPS, with authentication and authorization):

```sh
kubectl create clusterrolebinding platform-operator-metrics \
  --clusterrole=platform-operator-metrics-reader \
  --serviceaccount=platform-operator-system:platform-operator-controller-manager
TOKEN=$(kubectl create token platform-operator-controller-manager -n platform-operator-system)
kubectl port-forward -n platform-operator-system svc/platform-operator-controller-manager-metrics-service 8443:8443 &
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/metrics | grep '^platform_'
```

## Things to try

Watch in one terminal while you poke in another:

```sh
kubectl get webapps -w
```

| Try | What to watch for |
|---|---|
| `kubectl describe webapp storefront` | Conditions, events (`RolloutTriggered`, …) |
| `kubectl get deploy,svc,cm,ingress -l app.kubernetes.io/instance=storefront` | The children it created |
| `kubectl get deploy storefront -o yaml \| grep -A8 ownerReferences` | The owner reference |
| `kubectl edit webapp storefront`, change `config.LOG_LEVEL` | New `config-hash` annotation → rolling update (`kubectl rollout status deploy/storefront`) |
| Rotate the Secret: `kubectl create secret generic storefront-secrets --from-literal=PAYMENT_API_KEY=v2 --dry-run=client -o yaml \| kubectl apply -f -` | Same: pods roll |
| `kubectl delete deploy storefront` | Recreated within a second (self-healing via `Owns`) |
| `kubectl scale webapp storefront --replicas=5` | The scale subresource; the schedule then puts it back |
| `kubectl get ss storefront-hours -o yaml` | `activeWindow`, `nextTransitionTime` |
| Edit a schedule window to cover *now* | Replicas change on the next reconcile |
| `kubectl apply` a WebApp with `image: nginx` | Webhook warning (Option B only) |
| `kubectl apply` a WebApp with env `PORT` | CEL rejection (both options) |
| `kubectl get cm webapp-catalog -n platform-operator-system -o yaml` | Catalog entries (`-n default` with Option A) |
| `kubectl delete webapp storefront` then quickly `kubectl get webapp storefront -o yaml` | `deletionTimestamp` + finalizer, then gone; catalog entry removed |
| `kubectl get platform` | The `categories` marker lists both kinds |
| `kubectl explain webapp.spec` | Your Go doc comments, served by the API server |

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `no matches for kind "WebApp"` | CRDs not installed: `make install` |
| `failed calling webhook ... connection refused` | Webhook configs installed but operator not running/ready (`kubectl get pods -n platform-operator-system`) or cert-manager not ready |
| `x509: certificate signed by unknown authority` (webhook) | cert-manager CA injection not done yet. Wait, or check `kubectl get certificate -A` |
| Operator logs: `... is forbidden: User "system:serviceaccount:..."` | Missing `+kubebuilder:rbac` marker → add it, `make manifests`, redeploy |
| `ErrImagePull` for the operator | Used a `:latest` tag with kind; use `make dev-deploy` (tags `:dev`) |
| WebApp stuck `Terminating` | Operator not running to remove the finalizer. Start it, or remove the finalizer by hand ([04](04-webapp-controller.md#2-finalizers)) |
| `Degraded=True, SecretNotFound` | Create the Secret named in `spec.secretRef` |
| WebApp `Available=False` forever | Look at the pods: `kubectl get pods -l app.kubernetes.io/instance=<name>`. An image that doesn't listen on `spec.port` fails the readiness probe. |
| Need more detail in the logs | `ENABLE_WEBHOOKS=false go run ./cmd/main.go --zap-log-level=debug` (in-cluster: add the flag to `args` in `config/manager/manager.yaml`) |

## Cleaning up (order matters)

```sh
kubectl delete -k config/samples   # 1. delete CRs while the operator can still run finalizers
make undeploy                      # 2. operator, RBAC, webhooks, CRDs
make dev-cluster-delete            # 3. the whole kind cluster (makes 1–2 moot)
```

## Shipping a release

```sh
make docker-build docker-push IMG=registry.example.com/platform-operator:v0.1.0
make build-installer IMG=registry.example.com/platform-operator:v0.1.0   # → dist/install.yaml
kubectl apply -f dist/install.yaml   # what users run (cert-manager must already be installed)
```

Next: [9. Production checklist](09-production-checklist.md)
