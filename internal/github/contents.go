// Deployment statuses and repository content. Workflow discovery reads these:
// the platform has no checkout of the repo when a webhook arrives.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// DeploymentStatusRequest is the body of a deployment status write.
type DeploymentStatusRequest struct {
	State          string `json:"state"`
	Description    string `json:"description,omitempty"`
	LogURL         string `json:"log_url,omitempty"`
	EnvironmentURL string `json:"environment_url,omitempty"`
	Environment    string `json:"environment,omitempty"`
	AutoInactive   *bool  `json:"auto_inactive,omitempty"`
}

// DeploymentStatus is the created status.
type DeploymentStatus struct {
	ID          int64     `json:"id"`
	State       string    `json:"state"`
	Description string    `json:"description"`
	Environment string    `json:"environment"`
	LogURL      string    `json:"log_url"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateDeploymentStatus posts a status against a deployment.
func (c *Client) CreateDeploymentStatus(ctx context.Context, repo Repo, deploymentID int64, req DeploymentStatusRequest) (*DeploymentStatus, error) {
	if !repo.Valid() {
		return nil, fmt.Errorf("github: CreateDeploymentStatus needs owner and name, got %q", repo)
	}
	if deploymentID == 0 {
		return nil, errors.New("github: CreateDeploymentStatus needs a deployment id")
	}
	if req.State == "" {
		return nil, errors.New("github: CreateDeploymentStatus needs a state")
	}
	var out DeploymentStatus
	path := fmt.Sprintf("%s/deployments/%d/statuses", repo.path(), deploymentID)
	if _, err := c.Post(ctx, path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FileContent is one file fetched at a ref, already decoded.
type FileContent struct {
	Path    string
	SHA     string
	Size    int64
	Content []byte
	HTMLURL string
}

type contentEntry struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	SHA         string `json:"sha"`
	Size        int64  `json:"size"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	HTMLURL     string `json:"html_url"`
	DownloadURL string `json:"download_url"`
}

// GetFileContents fetches one file at a ref and decodes it. A directory, or a
// file too large for the contents API to inline, is an error naming the reason
// rather than empty bytes.
func (c *Client) GetFileContents(ctx context.Context, repo Repo, path, ref string) (*FileContent, error) {
	if !repo.Valid() {
		return nil, fmt.Errorf("github: GetFileContents needs owner and name, got %q", repo)
	}
	if path == "" {
		return nil, errors.New("github: GetFileContents needs a path")
	}
	endpoint := repo.path() + "/contents/" + escapePath(path)
	if ref != "" {
		endpoint += "?ref=" + url.QueryEscape(ref)
	}
	var entry contentEntry
	if _, err := c.Get(ctx, endpoint, &entry); err != nil {
		return nil, err
	}
	if entry.Type == "dir" {
		return nil, fmt.Errorf("github: %s:%s is a directory, not a file", repo, path)
	}
	if entry.Encoding != "base64" {
		return nil, fmt.Errorf("github: %s:%s came back with encoding %q (files over 1MB are not inlined by the contents API)", repo, path, entry.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(entry.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decoding %s:%s: %w", repo, path, err)
	}
	return &FileContent{Path: entry.Path, SHA: entry.SHA, Size: entry.Size, Content: decoded, HTMLURL: entry.HTMLURL}, nil
}

// WorkflowFile is one discovered workflow definition.
type WorkflowFile struct {
	Path string
	Name string
	SHA  string
	Size int64
}

// WorkflowsDir is where workflow definitions live.
const WorkflowsDir = ".github/workflows"

// ListWorkflowFiles lists the .yml/.yaml files under .github/workflows at a
// ref. A repo without that directory yields ErrNotFound, so the caller decides
// what "no workflows" means rather than receiving a silent empty list.
func (c *Client) ListWorkflowFiles(ctx context.Context, repo Repo, ref string) ([]WorkflowFile, error) {
	if !repo.Valid() {
		return nil, fmt.Errorf("github: ListWorkflowFiles needs owner and name, got %q", repo)
	}
	endpoint := repo.path() + "/contents/" + escapePath(WorkflowsDir)
	if ref != "" {
		endpoint += "?ref=" + url.QueryEscape(ref)
	}
	var entries []contentEntry
	if _, err := c.Get(ctx, endpoint, &entries); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s has no %s at ref %q", ErrNotFound, repo, WorkflowsDir, ref)
		}
		return nil, err
	}
	var out []WorkflowFile
	for _, e := range entries {
		if e.Type != "file" || !isWorkflowYAML(e.Name) {
			continue
		}
		out = append(out, WorkflowFile{Path: e.Path, Name: e.Name, SHA: e.SHA, Size: e.Size})
	}
	return out, nil
}

func isWorkflowYAML(name string) bool {
	l := strings.ToLower(name)
	return strings.HasSuffix(l, ".yml") || strings.HasSuffix(l, ".yaml")
}

// escapePath escapes each path segment, keeping the separators.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
