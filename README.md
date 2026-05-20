# Firebase Domain Controller

A Kubernetes controller that keeps Firebase authorized domains in sync with your Ingress hosts.

## The Problem

Firebase Authentication requires every domain your app runs on to be in the "Authorized domains" list. When you have dynamic environments (PR previews, feature branches, SLUG-based deploys), manually managing this list doesn't scale. Domains get added but never cleaned up, or new environments break because someone forgot to add the domain.

## What This Does

This controller watches Ingress resources in your cluster. When it sees an Ingress with a specific annotation, it automatically:
- **Adds** all `spec.rules[].host` domains to Firebase authorized domains
- **Tracks** which domains it added (so it never touches manually-added ones)
- **Removes** those domains when the Ingress is deleted

It uses a Kubernetes finalizer to guarantee cleanup happens even during namespace deletion.

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

## How It Works

```
Ingress created/updated with annotation
         |
         v
Controller extracts hosts from spec.rules
         |
         v
Adds NEW domains to Firebase (skips existing ones)
         |
         v
Tracks added domains in managed-domains annotation
Adds firebase-cleanup finalizer
         |
         v
(later) Ingress deleted or namespace deleted
         |
         v
Finalizer blocks deletion
Controller removes ONLY domains it tracked
Removes finalizer -> deletion completes
```

**Key safety property:** The controller only removes domains it added. Domains added manually through the Firebase console are never touched.

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
