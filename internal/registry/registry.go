// Package registry is a minimal OCI Distribution (registry v2) client used to
// list container images WITHOUT going through Forgejo. It speaks the standard
// `/v2/_catalog` + `/v2/<name>/tags/list` endpoints, so it works against
// artifact (the in-cluster registry) as well as any OCI-conformant registry.
//
// This replaces the Forgejo packages API for ListOCIImages, so the workspace
// deployment can source images, builds and the catalog all from ONE registry
// (artifact) while Forgejo stays the git host.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client is a minimal registry v2 client.
type Client struct {
	// Host is the registry host, e.g. artifact.worker.svc.cluster.local.
	Host string
	// Scheme is "http" (the in-cluster plaintext registry) or "https" (default).
	Scheme string
	// User/Pass are Basic-auth credentials (empty = anonymous). artifact
	// requires auth for the catalog even though blob/manifest pulls are
	// anonymous.
	User string
	Pass string
	// HC is the HTTP client (default: a 30s-timeout client).
	HC *http.Client
}

// Image is one repo tag under an owner namespace.
type Image struct {
	Owner string
	Name  string
	Tag   string
}

func (c *Client) scheme() string {
	if c.Scheme != "" {
		return c.Scheme
	}
	return "https"
}

func (c *Client) hc() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// get performs an authenticated GET and decodes the JSON body, returning the
// `Link` header (RFC 5988 pagination) verbatim.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) (string, error) {
	u := c.scheme() + "://" + strings.TrimRight(c.Host, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if c.User != "" || c.Pass != "" {
		req.SetBasicAuth(c.User, c.Pass)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", err
	}
	return resp.Header.Get("Link"), nil
}

// catalog lists every repository in the registry (paginated).
func (c *Client) catalog(ctx context.Context) ([]string, error) {
	var repos []string
	path := "/v2/_catalog"
	q := url.Values{}
	q.Set("n", "1000")
	for {
		var page struct {
			Repositories []string `json:"repositories"`
		}
		link, err := c.get(ctx, path, q, &page)
		if err != nil {
			return nil, err
		}
		repos = append(repos, page.Repositories...)
		next := nextLink(link)
		if next == "" {
			break
		}
		path, q = splitRelative(next)
	}
	return repos, nil
}

// tags lists a repository's tags (paginated).
func (c *Client) tags(ctx context.Context, repo string) ([]string, error) {
	var tags []string
	path := "/v2/" + strings.Trim(repo, "/") + "/tags/list"
	q := url.Values{}
	q.Set("n", "1000")
	for {
		var page struct {
			Tags []string `json:"tags"`
		}
		link, err := c.get(ctx, path, q, &page)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page.Tags...)
		next := nextLink(link)
		if next == "" {
			break
		}
		path, q = splitRelative(next)
	}
	return tags, nil
}

// ListImages lists the tags of every repository under `owner`. When `name` is
// non-empty only that image's tags are returned. Digest pseudo-tags
// (sha256:...) are skipped — only real, referenceable tags are surfaced.
func (c *Client) ListImages(ctx context.Context, owner, name string) ([]Image, error) {
	repos, err := c.catalog(ctx)
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(owner, "/") + "/"
	out := []Image{}
	for _, full := range repos {
		if !strings.HasPrefix(full, prefix) {
			continue
		}
		img := strings.TrimPrefix(full, prefix)
		if img == "" || (name != "" && img != name) {
			continue
		}
		ts, err := c.tags(ctx, full)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if strings.HasPrefix(t, "sha256:") {
				continue
			}
			out = append(out, Image{Owner: owner, Name: img, Tag: t})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// nextLink extracts the rel="next" URL from an RFC 5988 Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 || !strings.Contains(seg[1], `rel="next"`) {
			continue
		}
		return strings.Trim(strings.TrimSpace(seg[0]), "<>")
	}
	return ""
}

// splitRelative splits an absolute-or-relative URL into (path, query).
func splitRelative(ref string) (string, url.Values) {
	if u, err := url.Parse(ref); err == nil {
		return u.Path, u.Query()
	}
	return ref, url.Values{}
}
