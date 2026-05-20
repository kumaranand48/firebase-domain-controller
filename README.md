# Firebase Domain Controller

A Kubernetes controller that keeps Firebase authorized domains in sync with your Ingress hosts.

## The Problem

Firebase Authentication requires every domain your app runs on to be in the "Authorized domains" list. When you have dynamic environments (PR previews, feature branches, SLUG-based deploys), manually managing this list doesn't scale. Domains get added but never cleaned up, or new environments break because someone forgot to add the domain.

## What This Does

Add this annotation to any Ingress you want the controller to manage:

```yaml
annotations:
  <your-annotation-domain>/firebase-sync: "true"
```

The `<your-annotation-domain>` is configured when you deploy the controller (e.g. `controller.example.com`).

```
┌──────────────────────────────────────────────────────┐
│  SYNC: Ingress created or updated                    │
├──────────────────────────────────────────────────────┤
│                                                      │
│  1. Ingress has firebase-sync: "true" annotation?    │
│     NO  → skip                                       │
│     YES ↓                                            │
│                                                      │
│  2. Extract hosts from spec.rules[].host             │
│     e.g. ["app.example.com", "api.example.com"]      │
│                                                      │
│  3. For each host:                                   │
│     - Already in Firebase? → skip                    │
│     - New? → add to Firebase                         │
│                                                      │
│  4. Store added domains in managed-domains annotation │
│     Add firebase-cleanup finalizer                   │
│                                                      │
└──────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────┐
│  CLEANUP: Ingress or namespace deleted               │
├──────────────────────────────────────────────────────┤
│                                                      │
│  1. Finalizer blocks Kubernetes from deleting        │
│                                                      │
│  2. Read managed-domains annotation                  │
│     → only domains THIS controller added             │
│                                                      │
│  3. Remove those domains from Firebase               │
│     (manually-added domains are never touched)       │
│                                                      │
│  4. Remove finalizer → deletion completes            │
│                                                      │
└──────────────────────────────────────────────────────┘
```

## Quick Start

### 1. Create the secret

Download your Firebase service account JSON from Firebase Console -> Project Settings -> Service Accounts -> Generate new private key.

```bash
kubectl create namespace devops

kubectl create secret generic firebase-credentials \
  --from-file=service-account.json=/path/to/your-firebase-service-account.json \
  -n devops
```

The controller reads the `project_id` from this file automatically.

### 2. Configure the deployment

Edit `k8s/deployment.yaml` and set two things:

```yaml
# Your container registry
image: ghcr.io/your-org/firebase-domain-controller:latest

# Your annotation domain prefix (REQUIRED — no default)
env:
  - name: ANNOTATION_DOMAIN
    value: "controller.mycompany.com"
```

This determines the annotation keys:
- `controller.mycompany.com/firebase-sync` — opt-in annotation on Ingress
- `controller.mycompany.com/managed-domains` — tracks domains the controller owns
- `controller.mycompany.com/firebase-cleanup` — finalizer for safe deletion

### 3. Deploy

```bash
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/deployment.yaml
```

### 4. Annotate your Ingresses

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app
  annotations:
    controller.mycompany.com/firebase-sync: "true"
spec:
  rules:
  - host: my-app.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: my-service
            port:
              number: 80
```

The controller picks up the Ingress, adds `my-app.example.com` to Firebase, and removes it when the Ingress is deleted.

### Local development

```bash
go run main.go \
  --kubeconfig=$HOME/.kube/config \
  --firebase-creds=/path/to/service-account.json \
  --annotation-domain=controller.example.com
```

## Configuration

| Flag | Env Var | Description | Default |
|------|---------|-------------|---------|
| `--annotation-domain` | `ANNOTATION_DOMAIN` | Domain prefix for annotations/finalizers (**required**) | — |
| `--firebase-creds` | — | Path to Firebase service account JSON | `/etc/firebase/service-account.json` |
| `--firebase-project-id` | `FIREBASE_PROJECT_ID` | Firebase project ID | Read from service account JSON |
| `--kubeconfig` | — | Path to kubeconfig (local dev only) | In-cluster config |
| `--v` | — | Log verbosity (0-4) | `0` |

## Building

```bash
docker build -t your-registry/firebase-domain-controller:latest .
docker push your-registry/firebase-domain-controller:latest
```

## RBAC

The controller needs minimal permissions:
- `get`, `list`, `watch`, `update` on Ingresses (update for annotations + finalizers)
- `create`, `patch` on Events

See `k8s/rbac.yaml`.

## License

Apache 2.0
