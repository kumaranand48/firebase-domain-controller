package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"firebase-domain-controller/firebase"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	networkinglister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// Max retries for processing items
	maxRetries = 5
)

// annotationDomain is the domain prefix used for annotations and finalizers.
// Set via Config.AnnotationDomain (e.g. "controller.example.com" produces
// "controller.example.com/firebase-sync", etc.)
var (
	syncAnnotation           string
	managedDomainsAnnotation string
	finalizerName            string
)

func initAnnotations(domain string) {
	syncAnnotation = domain + "/firebase-sync"
	managedDomainsAnnotation = domain + "/managed-domains"
	finalizerName = domain + "/firebase-cleanup"
}

// Controller watches Ingress resources and syncs domains with Firebase
type Controller struct {
	clientset       kubernetes.Interface
	firebaseClient  *firebase.Client
	informerFactory informers.SharedInformerFactory
	ingressLister   networkinglister.IngressLister
	ingressSynced   cache.InformerSynced
	workqueue       workqueue.RateLimitingInterface
}

// NewController creates a new controller instance.
// annotationDomain sets the domain prefix for annotations/finalizers
// (e.g. "controller.example.com").
func NewController(
	clientset kubernetes.Interface,
	firebaseClient *firebase.Client,
	annotationDomain string,
) *Controller {
	initAnnotations(annotationDomain)
	informerFactory := informers.NewSharedInformerFactory(clientset, time.Minute*10)
	ingressInformer := informerFactory.Networking().V1().Ingresses()

	controller := &Controller{
		clientset:       clientset,
		firebaseClient:  firebaseClient,
		informerFactory: informerFactory,
		ingressLister:   ingressInformer.Lister(),
		ingressSynced:   ingressInformer.Informer().HasSynced,
		workqueue: workqueue.NewNamedRateLimitingQueue(
			workqueue.DefaultControllerRateLimiter(),
			"Ingresses",
		),
	}

	klog.Info("Setting up Ingress event handlers")

	// Add event handlers for Ingress resources
	ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleAdd,
		UpdateFunc: func(old, new interface{}) {
			controller.handleUpdate(old, new)
		},
		DeleteFunc: controller.handleDelete,
	})

	return controller
}

// Run starts the controller
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("Starting Firebase Domain Controller")

	// Start informer factory
	klog.Info("Starting informer factory")
	c.informerFactory.Start(ctx.Done())

	klog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(ctx.Done(), c.ingressSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Infof("Starting %d workers", workers)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	klog.Info("Started workers")
	<-ctx.Done()
	klog.Info("Shutting down workers")

	return nil
}

// runWorker processes items from the workqueue
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem processes a single item from the workqueue
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)

		key, ok := obj.(string)
		if !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.syncHandler(ctx, key); err != nil {
			if c.workqueue.NumRequeues(key) < maxRetries {
				c.workqueue.AddRateLimited(key)
				return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
			}
			c.workqueue.Forget(key)
			utilruntime.HandleError(fmt.Errorf("dropping ingress %q out of the queue: %v", key, err))
			return nil
		}

		c.workqueue.Forget(obj)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler processes a single Ingress resource
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	ingress, err := c.ingressLister.Ingresses(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).Infof("Ingress %s has been deleted", key)
			return nil
		}
		return err
	}

	// Check if the Ingress has the sync annotation
	if !c.shouldSync(ingress) {
		klog.V(4).Infof("Ingress %s does not have sync annotation, skipping", key)
		return nil
	}

	// Check if Ingress is being deleted
	if ingress.DeletionTimestamp != nil {
		return c.handleFinalization(ctx, ingress)
	}

	// Extract domains from Ingress
	domains := c.extractDomains(ingress)
	if len(domains) == 0 {
		klog.Infof("No domains found in Ingress %s", key)
		return nil
	}

	// Get currently managed domains
	managedDomains := c.getManagedDomains(ingress)

	// Check which domains we need to add (only new ones)
	domainsToAdd := []string{}
	for _, domain := range domains {
		// Check if we're already managing this domain
		alreadyManaged := false
		for _, managed := range managedDomains {
			if managed == domain {
				alreadyManaged = true
				break
			}
		}
		if !alreadyManaged {
			domainsToAdd = append(domainsToAdd, domain)
		}
	}

	// Add new domains to Firebase and track them
	if len(domainsToAdd) > 0 {
		for _, domain := range domainsToAdd {
			if err := c.firebaseClient.AddDomain(ctx, domain); err != nil {
				return fmt.Errorf("failed to add domain %s: %w", domain, err)
			}
			managedDomains = append(managedDomains, domain)
			klog.Infof("Added and tracking domain: %s", domain)
		}

		// Update the managed domains annotation
		if err := c.updateManagedDomains(ctx, ingress, managedDomains); err != nil {
			return fmt.Errorf("failed to update managed domains annotation: %w", err)
		}
	}

	// Ensure finalizer is present
	if !c.hasFinalizer(ingress) {
		if err := c.addFinalizer(ctx, ingress); err != nil {
			return fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	return nil
}

// handleAdd handles Ingress add events
func (c *Controller) handleAdd(obj interface{}) {
	ingress := obj.(*networkingv1.Ingress)
	if c.shouldSync(ingress) {
		key, err := cache.MetaNamespaceKeyFunc(obj)
		if err == nil {
			klog.Infof("Ingress added: %s", key)
			c.workqueue.Add(key)
		}
	}
}

// handleUpdate handles Ingress update events
func (c *Controller) handleUpdate(oldObj, newObj interface{}) {
	oldIngress := oldObj.(*networkingv1.Ingress)
	newIngress := newObj.(*networkingv1.Ingress)

	if oldIngress.ResourceVersion == newIngress.ResourceVersion {
		return
	}

	if c.shouldSync(newIngress) {
		key, err := cache.MetaNamespaceKeyFunc(newObj)
		if err == nil {
			klog.Infof("Ingress updated: %s", key)
			c.workqueue.Add(key)
		}
	}
}

// handleDelete handles Ingress delete events
// NOTE: With finalizers, this will be called AFTER our finalizer cleanup completes
func (c *Controller) handleDelete(obj interface{}) {
	ingress, ok := obj.(*networkingv1.Ingress)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ingress, ok = tombstone.Obj.(*networkingv1.Ingress)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not an Ingress %#v", obj))
			return
		}
	}

	if c.shouldSync(ingress) {
		key, _ := cache.MetaNamespaceKeyFunc(ingress)
		klog.Infof("Ingress deleted (after finalization): %s", key)
		// Cleanup is already done by the finalizer, nothing to do here
	}
}

// shouldSync checks if the Ingress should be synced
func (c *Controller) shouldSync(ingress *networkingv1.Ingress) bool {
	if ingress.Annotations == nil {
		return false
	}
	value, exists := ingress.Annotations[syncAnnotation]
	return exists && value == "true"
}

// extractDomains extracts all domains from an Ingress resource
func (c *Controller) extractDomains(ingress *networkingv1.Ingress) []string {
	var domains []string
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			domains = append(domains, rule.Host)
		}
	}
	return domains
}

// GetIngress retrieves an Ingress by namespace and name
func (c *Controller) GetIngress(namespace, name string) (*networkingv1.Ingress, error) {
	return c.clientset.NetworkingV1().Ingresses(namespace).Get(
		context.Background(),
		name,
		metav1.GetOptions{},
	)
}

// handleFinalization handles cleanup when an Ingress is being deleted
func (c *Controller) handleFinalization(ctx context.Context, ingress *networkingv1.Ingress) error {
	key := fmt.Sprintf("%s/%s", ingress.Namespace, ingress.Name)

	// Check if our finalizer is present
	if !c.hasFinalizer(ingress) {
		klog.V(4).Infof("Ingress %s does not have our finalizer, skipping cleanup", key)
		return nil
	}

	// Get the domains WE added (tracked in annotation)
	managedDomains := c.getManagedDomains(ingress)
	if len(managedDomains) == 0 {
		klog.Infof("No managed domains to cleanup for Ingress %s", key)
	} else {
		klog.Infof("Removing %d managed domains from Firebase for Ingress %s", len(managedDomains), key)

		// Only remove domains WE added
		for _, domain := range managedDomains {
			if err := c.firebaseClient.RemoveDomain(ctx, domain); err != nil {
				klog.Errorf("Failed to remove managed domain %s: %v", domain, err)
				// Don't return error - continue with other domains
			} else {
				klog.Infof("Successfully removed managed domain: %s", domain)
			}
		}
	}

	// Remove our finalizer
	if err := c.removeFinalizer(ctx, ingress); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	klog.Infof("Finalization complete for Ingress %s", key)
	return nil
}

// getManagedDomains retrieves the list of domains managed by this controller
func (c *Controller) getManagedDomains(ingress *networkingv1.Ingress) []string {
	if ingress.Annotations == nil {
		return []string{}
	}

	domainsJSON, exists := ingress.Annotations[managedDomainsAnnotation]
	if !exists || domainsJSON == "" {
		return []string{}
	}

	var domains []string
	if err := json.Unmarshal([]byte(domainsJSON), &domains); err != nil {
		klog.Errorf("Failed to unmarshal managed domains: %v", err)
		return []string{}
	}

	return domains
}

// updateManagedDomains updates the managed domains annotation
func (c *Controller) updateManagedDomains(ctx context.Context, ingress *networkingv1.Ingress, domains []string) error {
	domainsJSON, err := json.Marshal(domains)
	if err != nil {
		return fmt.Errorf("failed to marshal domains: %w", err)
	}

	// Get fresh copy of Ingress
	freshIngress, err := c.clientset.NetworkingV1().Ingresses(ingress.Namespace).Get(
		ctx,
		ingress.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get fresh Ingress: %w", err)
	}

	if freshIngress.Annotations == nil {
		freshIngress.Annotations = make(map[string]string)
	}
	freshIngress.Annotations[managedDomainsAnnotation] = string(domainsJSON)

	_, err = c.clientset.NetworkingV1().Ingresses(freshIngress.Namespace).Update(
		ctx,
		freshIngress,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to update Ingress annotations: %w", err)
	}

	klog.V(4).Infof("Updated managed domains for Ingress %s/%s: %v", ingress.Namespace, ingress.Name, domains)
	return nil
}

// hasFinalizer checks if the Ingress has our finalizer
func (c *Controller) hasFinalizer(ingress *networkingv1.Ingress) bool {
	for _, finalizer := range ingress.Finalizers {
		if finalizer == finalizerName {
			return true
		}
	}
	return false
}

// addFinalizer adds our finalizer to the Ingress
func (c *Controller) addFinalizer(ctx context.Context, ingress *networkingv1.Ingress) error {
	// Get fresh copy of Ingress
	freshIngress, err := c.clientset.NetworkingV1().Ingresses(ingress.Namespace).Get(
		ctx,
		ingress.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get fresh Ingress: %w", err)
	}

	// Check if finalizer already exists (race condition)
	if c.hasFinalizer(freshIngress) {
		return nil
	}

	freshIngress.Finalizers = append(freshIngress.Finalizers, finalizerName)

	_, err = c.clientset.NetworkingV1().Ingresses(freshIngress.Namespace).Update(
		ctx,
		freshIngress,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to add finalizer: %w", err)
	}

	klog.Infof("Added finalizer to Ingress %s/%s", ingress.Namespace, ingress.Name)
	return nil
}

// removeFinalizer removes our finalizer from the Ingress
func (c *Controller) removeFinalizer(ctx context.Context, ingress *networkingv1.Ingress) error {
	// Get fresh copy of Ingress
	freshIngress, err := c.clientset.NetworkingV1().Ingresses(ingress.Namespace).Get(
		ctx,
		ingress.Name,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to get fresh Ingress: %w", err)
	}

	// Remove our finalizer
	finalizers := []string{}
	for _, finalizer := range freshIngress.Finalizers {
		if finalizer != finalizerName {
			finalizers = append(finalizers, finalizer)
		}
	}

	if len(finalizers) == len(freshIngress.Finalizers) {
		// Finalizer not found, nothing to do
		return nil
	}

	freshIngress.Finalizers = finalizers

	_, err = c.clientset.NetworkingV1().Ingresses(freshIngress.Namespace).Update(
		ctx,
		freshIngress,
		metav1.UpdateOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	klog.Infof("Removed finalizer from Ingress %s/%s", ingress.Namespace, ingress.Name)
	return nil
}
