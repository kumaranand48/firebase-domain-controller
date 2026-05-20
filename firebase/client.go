package firebase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"k8s.io/klog/v2"
)

const (
	identityToolkitAPI = "https://identitytoolkit.googleapis.com/v2/projects/%s/config"
	firebaseScope      = "https://www.googleapis.com/auth/cloud-platform"
)

// Client manages Firebase authorized domains
type Client struct {
	projectID   string
	httpClient  *http.Client
	tokenSource oauth2.TokenSource
	mu          sync.Mutex
}

// Config represents the Firebase project configuration
type Config struct {
	AuthorizedDomains []string `json:"authorizedDomains,omitempty"`
}

// ServiceAccountKey represents the structure of a Firebase service account JSON
type ServiceAccountKey struct {
	ProjectID string `json:"project_id"`
}

// NewClient creates a new Firebase client
func NewClient(ctx context.Context, credentialsPath, projectID string) (*Client, error) {
	// Read service account credentials
	credBytes, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read Firebase credentials: %w", err)
	}

	// If projectID not provided, parse it from the credentials JSON
	if projectID == "" {
		var serviceAccount ServiceAccountKey
		if err := json.Unmarshal(credBytes, &serviceAccount); err != nil {
			return nil, fmt.Errorf("failed to parse service account JSON: %w", err)
		}
		projectID = serviceAccount.ProjectID
		if projectID == "" {
			return nil, fmt.Errorf("project_id not found in service account JSON")
		}
		klog.Infof("Using project ID from service account JSON: %s", projectID)
	}

	// Create OAuth2 config
	creds, err := google.CredentialsFromJSON(ctx, credBytes, firebaseScope)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Firebase credentials: %w", err)
	}

	client := &Client{
		projectID:   projectID,
		httpClient:  oauth2.NewClient(ctx, creds.TokenSource),
		tokenSource: creds.TokenSource,
	}

	klog.Infof("Firebase client initialized for project: %s", projectID)
	return client, nil
}

// AddDomain adds a domain to Firebase authorized domains
func (c *Client) AddDomain(ctx context.Context, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	klog.Infof("Adding domain to Firebase: %s", domain)

	// Get current authorized domains
	config, err := c.getConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current config: %w", err)
	}

	// Check if domain already exists
	for _, d := range config.AuthorizedDomains {
		if d == domain {
			klog.Infof("Domain %s already exists in authorized domains", domain)
			return nil
		}
	}

	// Add new domain
	config.AuthorizedDomains = append(config.AuthorizedDomains, domain)

	// Update config
	if err := c.updateConfig(ctx, config); err != nil {
		return fmt.Errorf("failed to update config: %w", err)
	}

	klog.Infof("Successfully added domain: %s", domain)
	return nil
}

// RemoveDomain removes a domain from Firebase authorized domains
func (c *Client) RemoveDomain(ctx context.Context, domain string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	klog.Infof("Removing domain from Firebase: %s", domain)

	// Get current authorized domains
	config, err := c.getConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current config: %w", err)
	}

	// Find and remove domain
	found := false
	newDomains := make([]string, 0, len(config.AuthorizedDomains))
	for _, d := range config.AuthorizedDomains {
		if d == domain {
			found = true
			continue
		}
		newDomains = append(newDomains, d)
	}

	if !found {
		klog.Infof("Domain %s not found in authorized domains", domain)
		return nil
	}

	config.AuthorizedDomains = newDomains

	// Update config
	if err := c.updateConfig(ctx, config); err != nil {
		return fmt.Errorf("failed to update config: %w", err)
	}

	klog.Infof("Successfully removed domain: %s", domain)
	return nil
}

// getConfig retrieves the current Firebase project configuration
func (c *Client) getConfig(ctx context.Context) (*Config, error) {
	url := fmt.Sprintf(identityToolkitAPI, c.projectID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var config Config
	if err := json.NewDecoder(resp.Body).Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}

	return &config, nil
}

// updateConfig updates the Firebase project configuration
func (c *Client) updateConfig(ctx context.Context, config *Config) error {
	url := fmt.Sprintf(identityToolkitAPI+"?updateMask=authorizedDomains", c.projectID)

	payload, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Retry logic for transient failures
	maxRetries := 3
	var lastErr error

	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(time.Second * time.Duration(i))
			klog.Infof("Retrying Firebase update (attempt %d/%d)", i+1, maxRetries)
		}

		req, err := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			return nil
		}

		body, _ := io.ReadAll(resp.Body)
		lastErr = fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))

		if resp.StatusCode < 500 {
			// Don't retry client errors
			return lastErr
		}
	}

	return fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
}

// Close cleans up the client resources
func (c *Client) Close() error {
	klog.Info("Firebase client closed")
	return nil
}
