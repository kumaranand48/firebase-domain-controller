package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"firebase-domain-controller/controller"
	"firebase-domain-controller/firebase"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func main() {
	var kubeconfig string
	var firebaseCredPath string
	var firebaseProjectID string
	var annotationDomain string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file (optional, for local development)")
	flag.StringVar(&firebaseCredPath, "firebase-creds", "/etc/firebase/service-account.json", "Path to Firebase service account JSON")
	flag.StringVar(&firebaseProjectID, "firebase-project-id", os.Getenv("FIREBASE_PROJECT_ID"), "Firebase project ID (optional, will be read from service account JSON if not provided)")
	flag.StringVar(&annotationDomain, "annotation-domain", os.Getenv("ANNOTATION_DOMAIN"), "Domain prefix for annotations/finalizers (e.g. controller.example.com)")
	flag.Parse()

	if annotationDomain == "" {
		klog.Fatal("Annotation domain is required (--annotation-domain or ANNOTATION_DOMAIN env var)")
	}

	klog.InitFlags(nil)

	// Create Kubernetes client
	config, err := buildConfig(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to build Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Initialize Firebase client
	fbClient, err := firebase.NewClient(context.Background(), firebaseCredPath, firebaseProjectID)
	if err != nil {
		klog.Fatalf("Failed to initialize Firebase client: %v", err)
	}
	defer fbClient.Close()

	klog.Info("Firebase Domain Controller started")

	// Create and start the controller
	ctrl := controller.NewController(clientset, fbClient, annotationDomain)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		klog.Info("Received shutdown signal, stopping controller...")
		cancel()
	}()

	// Run the controller
	if err := ctrl.Run(ctx, 2); err != nil {
		klog.Fatalf("Error running controller: %v", err)
	}

	klog.Info("Controller stopped")
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		// Use kubeconfig file (local development)
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	// Use in-cluster config (production)
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	// Set reasonable timeouts
	config.Timeout = 30 * time.Second

	return config, nil
}
