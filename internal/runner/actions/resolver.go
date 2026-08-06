package actions

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Resolved is a reference turned into something executable.
type Resolved struct {
	Ref Reference
	// Dir is the host directory holding the action, "" for docker and local
	// references, which the executor handles without a download.
	Dir string
	// SHA is the immutable commit the ref resolved to, for repo references.
	SHA string
	// Meta is the parsed action.yml, nil for docker and local references.
	Meta *Metadata
	// Cached is true when the tarball was already on disk.
	Cached bool
}

// Fetcher talks to the code host. It is an interface so the resolver is
// testable without a network.
type Fetcher interface {
	// ResolveRef maps a branch, tag, or sha to an immutable sha.
	ResolveRef(ctx context.Context, owner, repo, ref string) (string, error)
	// Tarball opens the repository archive at sha as a gzipped tar.
	Tarball(ctx context.Context, owner, repo, sha string) (io.ReadCloser, error)
}

// Resolver downloads and caches actions. The cache is content-addressed by
// owner/repo@sha, so a ref that already resolved is never downloaded twice.
type Resolver struct {
	CacheDir string
	Fetch    Fetcher

	mu   sync.Mutex
	refs map[string]string // owner/repo@ref -> sha, resolved once per process
}

// NewResolver returns a resolver writing into cacheDir.
func NewResolver(cacheDir string, f Fetcher) *Resolver {
	return &Resolver{CacheDir: cacheDir, Fetch: f, refs: map[string]string{}}
}

// Resolve turns a `uses:` value into a directory on disk. Local and docker
// references are returned parsed but unfetched: the executor owns those.
func (r *Resolver) Resolve(ctx context.Context, uses string) (Resolved, error) {
	ref, err := ParseReference(uses)
	if err != nil {
		return Resolved{}, err
	}
	switch ref.Kind {
	case KindLocal, KindDocker:
		return Resolved{Ref: ref}, nil
	}
	if r.Fetch == nil {
		return Resolved{}, fmt.Errorf("unable to resolve action %q: the runner has no action fetcher configured", ref.Text)
	}

	sha, err := r.resolveSHA(ctx, ref)
	if err != nil {
		return Resolved{}, fmt.Errorf("unable to resolve action %q: %w", ref.Text, err)
	}

	dir := filepath.Join(r.CacheDir, ref.Owner, ref.Repo, sha)
	cached := isPopulated(dir)
	if !cached {
		if err := r.download(ctx, ref, sha, dir); err != nil {
			return Resolved{}, fmt.Errorf("unable to resolve action %q: %w", ref.Text, err)
		}
	}

	actionDir := dir
	if ref.Path != "" {
		actionDir = filepath.Join(dir, filepath.FromSlash(ref.Path))
	}
	meta, err := LoadMetadataDir(actionDir)
	if err != nil {
		return Resolved{}, fmt.Errorf("action %q: %w", ref.Text, err)
	}
	return Resolved{Ref: ref, Dir: actionDir, SHA: sha, Meta: meta, Cached: cached}, nil
}

func (r *Resolver) resolveSHA(ctx context.Context, ref Reference) (string, error) {
	key := ref.Owner + "/" + ref.Repo + "@" + ref.Ref
	r.mu.Lock()
	sha, ok := r.refs[key]
	r.mu.Unlock()
	if ok {
		return sha, nil
	}
	sha, err := r.Fetch.ResolveRef(ctx, ref.Owner, ref.Repo, ref.Ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("reference %q does not exist", ref.Ref)
	}
	r.mu.Lock()
	r.refs[key] = sha
	r.mu.Unlock()
	return sha, nil
}

// marker is written last, so a half-extracted directory is never mistaken for
// a warm cache entry.
const marker = ".ci-platform-complete"

func isPopulated(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, marker))
	return err == nil
}

func (r *Resolver) download(ctx context.Context, ref Reference, sha, dir string) error {
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	rc, err := r.Fetch.Tarball(ctx, ref.Owner, ref.Repo, sha)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := extractTarGz(rc, tmp); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, marker), []byte(sha), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

// extractTarGz unpacks an archive, stripping the single wrapper directory that
// code hosts put around repository tarballs.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("action tarball: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("action tarball: %w", err)
		}
		name := stripLeading(h.Name)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func stripLeading(name string) string {
	name = filepath.ToSlash(strings.TrimPrefix(name, "./"))
	i := strings.Index(name, "/")
	if i < 0 {
		return ""
	}
	return name[i+1:]
}

// safeJoin refuses an archive entry that would escape the destination.
func safeJoin(dest, name string) (string, error) {
	target := filepath.Join(dest, filepath.FromSlash(name))
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("action tarball: entry %q escapes the extraction directory", name)
	}
	return target, nil
}

// LoadMetadataDir reads action.yml, falling back to action.yaml.
func LoadMetadataDir(dir string) (*Metadata, error) {
	var lastErr error
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			lastErr = err
			continue
		}
		return ParseMetadata(data)
	}
	return nil, fmt.Errorf("no action.yml or action.yaml in %s: %v", dir, lastErr)
}

// HTTPFetcher fetches from a GitHub-compatible REST API.
type HTTPFetcher struct {
	// BaseURL is the API root, e.g. https://api.github.com.
	BaseURL string
	Token   string
	Client  *http.Client
}

// NewHTTPFetcher returns a fetcher with a bounded client.
func NewHTTPFetcher(baseURL, token string) *HTTPFetcher {
	return &HTTPFetcher{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		Client:  &http.Client{Timeout: 2 * time.Minute},
	}
}

// ResolveRef resolves a ref via /repos/{owner}/{repo}/commits/{ref}.
func (f *HTTPFetcher) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/commits/%s", f.BaseURL, owner, repo, ref)
	resp, err := f.do(ctx, url, "application/vnd.github.sha")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") {
		var payload struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", fmt.Errorf("commit lookup returned unparseable JSON: %w", err)
		}
		return payload.SHA, nil
	}
	return trimmed, nil
}

// Tarball downloads /repos/{owner}/{repo}/tarball/{sha}.
func (f *HTTPFetcher) Tarball(ctx context.Context, owner, repo, sha string) (io.ReadCloser, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/tarball/%s", f.BaseURL, owner, repo, sha)
	resp, err := f.do(ctx, url, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (f *HTTPFetcher) do(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%s responded HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}
