.PHONY: help build test run docker-build docker-push deploy clean

# Variables
BINARY_NAME=firebase-domain-controller
DOCKER_IMAGE=your-registry/firebase-domain-controller
VERSION?=latest
NAMESPACE=devops

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

build: ## Build the Go binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -ldflags="-w -s" -o $(BINARY_NAME) .

test: ## Run tests
	go test -v ./...

run: ## Run locally (requires kubeconfig and Firebase credentials)
	@echo "Make sure you have:"
	@echo "  1. KUBECONFIG set or ~/.kube/config exists"
	@echo "  2. FIREBASE_PROJECT_ID env var set"
	@echo "  3. Firebase service account JSON at /tmp/firebase-creds.json"
	@echo ""
	go run main.go \
		--kubeconfig=${HOME}/.kube/config \
		--firebase-creds=/tmp/firebase-creds.json \
		--v=2

docker-build: ## Build Docker image
	docker build -t $(DOCKER_IMAGE):$(VERSION) .
	docker tag $(DOCKER_IMAGE):$(VERSION) $(DOCKER_IMAGE):latest

docker-push: docker-build ## Build and push Docker image
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest

deploy-rbac: ## Deploy RBAC resources
	kubectl apply -f k8s/rbac.yaml

deploy-dev: deploy-rbac ## Deploy to dev environment
	kubectl apply -f k8s/deployment.yaml
	kubectl rollout status deployment/firebase-domain-controller-dev -n $(NAMESPACE)

deploy-prod: deploy-rbac ## Deploy to prod environment
	kubectl apply -f k8s/deployment.yaml
	kubectl rollout status deployment/firebase-domain-controller-prod -n $(NAMESPACE)

logs-dev: ## View dev controller logs
	kubectl logs -f deployment/firebase-domain-controller-dev -n $(NAMESPACE)

logs-prod: ## View prod controller logs
	kubectl logs -f deployment/firebase-domain-controller-prod -n $(NAMESPACE)

restart-dev: ## Restart dev controller
	kubectl rollout restart deployment/firebase-domain-controller-dev -n $(NAMESPACE)

restart-prod: ## Restart prod controller
	kubectl rollout restart deployment/firebase-domain-controller-prod -n $(NAMESPACE)

clean: ## Clean build artifacts
	rm -f $(BINARY_NAME)
	go clean

mod-tidy: ## Tidy Go modules
	go mod tidy

fmt: ## Format Go code
	go fmt ./...

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## Run linters

all: lint test build docker-build ## Run all checks and build
