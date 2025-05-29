package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// IncomingPayload represents the incoming request structure
type IncomingPayload struct {
	Application string `json:"application"`
	GithubToken string `json:"github_token"`
}

// ValuesYAML represents the structure of the values.yaml file
type ValuesYAML struct {
	Global struct {
		Config struct {
			DeploymentTag string `yaml:"DEPLOYMENT_TAG"`
		} `yaml:"config"`
	} `yaml:"global"`
}

// GitHubDispatchPayload represents the payload sent to GitHub
type GitHubDispatchPayload struct {
	EventType     string                `json:"event_type"`
	ClientPayload DispatchClientPayload `json:"client_payload"`
}

type DispatchClientPayload struct {
	CommitHash string `json:"commitHash"`
	SourceName string `json:"sourceName"`
}

// Config holds the application configuration
type Config struct {
	Port              string
	GitHubAPIURL      string
	TargetRepo        string
	DeploymentRepo    string
	GitHubToken       string // Optional: for private repos
}

// Server holds the server dependencies
type Server struct {
	config     Config
	httpClient *http.Client
}

// NewServer creates a new server instance
func NewServer(config Config) *Server {
	return &Server{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// fetchValuesFile fetches the values.yaml file from GitHub
func (s *Server) fetchValuesFile(ctx context.Context, path string, githubToken string) ([]byte, error) {
	// Construct the raw content URL for GitHub
	// Format: https://raw.githubusercontent.com/{owner}/{repo}/{branch}/{path}
	parts := strings.Split(s.config.DeploymentRepo, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid repository URL format")
	}
	
	owner := parts[0]
	repo := parts[1]
	
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/master/%s",	owner, repo, path)
	
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	
	// Authorization token is required for private repos
	if githubToken != "" {
		req.Header.Set("Authorization", "token "+githubToken)
	} else {
		return nil, fmt.Errorf("no github token provided")
	}
	
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching file: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("file not found at %s", path)
	}
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	
	return io.ReadAll(resp.Body)
}

// extractDeploymentTag extracts the DEPLOYMENT_TAG from values.yaml
func (s *Server) extractDeploymentTag(content []byte) (string, error) {
	var values ValuesYAML
	
	if err := yaml.Unmarshal(content, &values); err != nil {
		return "", fmt.Errorf("parsing YAML: %w", err)
	}
	
	deploymentTag := values.Global.Config.DeploymentTag
	if deploymentTag == "" {
		return "", fmt.Errorf("DEPLOYMENT_TAG is empty or not found")
	}
	
	return deploymentTag, nil
}

// sendGitHubDispatch sends a repository dispatch event to GitHub
func (s *Server) sendGitHubDispatch(ctx context.Context, githubToken, commitHash, sourceName string) error {
	payload := GitHubDispatchPayload{
		EventType: "environment_ready",
		ClientPayload: DispatchClientPayload{
			CommitHash: commitHash,
			SourceName: sourceName,
		},	
	}
	
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling payload: %w", err)
	}
	
	url := fmt.Sprintf("%s/repos/%s/dispatches", s.config.GitHubAPIURL, s.config.TargetRepo)
	
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending dispatch: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}
	
	return nil
}

// handleDispatch handles the `/dispatch` endpoint
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// Parse request body
	var payload IncomingPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("Error decoding payload: %v", err)
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	
	applicationName := payload.Application
	
	// Check if application name starts with 'pr-'
	if !strings.HasPrefix(applicationName, "pr-") {
		log.Printf("Skipping application '%s' - doesn't start with 'pr-'", applicationName)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status": "skipped",
			"reason": "application name doesn't start with 'pr-'",
		})
		return
	}

	log.Printf("Dispatching for application '%s'", applicationName)
	
	ctx := r.Context()
	
	// Try to find values.yaml in light or heavy path
	paths := []string{
		fmt.Sprintf("configs/pr/light/%s/values.yaml", applicationName),
		fmt.Sprintf("configs/pr/heavy/%s/values.yaml", applicationName),
	}
	
	var githubToken string
	if s.config.GitHubToken != "" {
		githubToken = s.config.GitHubToken
	} else if payload.GithubToken != "" {
		githubToken = payload.GithubToken
	} else {
		http.Error(w, "A GitHub token is required", http.StatusBadRequest)
		return
	}

	var valuesContent []byte
	var foundPath string
	var err error
	for _, path := range paths {
		valuesContent, err = s.fetchValuesFile(ctx, path, githubToken)
		if err == nil {
			foundPath = path
			break
		}
	}
	
	if foundPath == "" {
		log.Printf("Could not find 'values.yaml' for application '%s'", applicationName)
		http.Error(w, fmt.Sprintf("Could not find 'values.yaml' for application '%s'", applicationName), http.StatusNotFound)
		return
	}
	log.Printf("Found values file at: %s", foundPath)
	
	commitHash, err := s.extractDeploymentTag(valuesContent)
	if err != nil {
		log.Printf("Error extracting deployment tag: %v", err)
		http.Error(w, fmt.Sprintf("Error extracting deployment tag: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Extracted SC commit hash: %s", commitHash)
	
	if err := s.sendGitHubDispatch(ctx, githubToken, commitHash, applicationName); err != nil {
		log.Printf("Error sending dispatch: %v", err)
		http.Error(w, fmt.Sprintf("Error sending dispatch: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("Successfully dispatched for application '%s' with commit hash '%s'", applicationName, commitHash)
	
	// Return success response
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(map[string]string{
		"status":     "success",
		"commitHash": commitHash,
		"sourceName": applicationName,
	})
	if err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// healthHandler handles health check endpoint
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func main() {
	// Load configuration from environment variables
	config := Config{
		Port:              getEnv("PORT", "9555"),
		GitHubAPIURL:      getEnv("GITHUB_API_URL", "https://api.github.com"),
		TargetRepo:        getEnv("TARGET_REPO", "sematext/sematext-cloud"),
		DeploymentRepo:     getEnv("DEPLOYMENT_REPO", "sematext/deployment"),
		GitHubToken:       getEnv("GITHUB_TOKEN", ""), // Can also be passed as a request parameter if not set here
	}
	
	// Create server
	server := NewServer(config)
	
	// Setup routes
	http.HandleFunc("/dispatch", server.handleDispatch)
	http.HandleFunc("/health", server.healthHandler)
	
	// Start server
	addr := ":" + config.Port
	log.Printf("Starting server on %s", addr)
	log.Printf("Configuration:")
	log.Printf("  Target Repository: %s", config.TargetRepo)
	log.Printf("  Deployment Repository: %s", config.DeploymentRepo)
	log.Printf("  GitHub API URL: %s", config.GitHubAPIURL)
	
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}

// getEnv gets an environment variable with a fallback default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}