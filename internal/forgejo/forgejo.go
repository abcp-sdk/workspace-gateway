// Package forgejo is a small typed client over the Forgejo (Gitea-compatible)
// REST API: read-only browse (orgs/repos/tree/blob/log/branches) plus the
// ensure-org/ensure-repo write path the gateway needs. It is intentionally
// minimal — the agent's repo-* tools own the actual editing.
package forgejo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one Forgejo instance with a shared token.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New builds a client. base may include a trailing slash.
func New(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// GitURL is the HTTP(S) clone/push URL for `org/repo`, derived from the API
// base. The gateway pushes import results here with the shared token.
func (c *Client) GitURL(org, repo string) string {
	return c.base + "/" + seg(org) + "/" + seg(repo) + ".git"
}

// Token exposes the shared API/git token (the import push authenticates with
// it). Never log the result.
func (c *Client) Token() string { return c.token }

// RepoInfo is one repository.
type RepoInfo struct {
	Org           string `json:"org"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// TreeEntry is one file/dir in a tree.
type TreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // file | dir
	Size int64  `json:"size"`
}

// CommitInfo is one commit.
type CommitInfo struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// BranchInfo is one branch.
type BranchInfo struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// ErrNotFound marks a 404.
type ErrNotFound struct{ URL string }

func (e *ErrNotFound) Error() string { return "forgejo: not found: " + e.URL }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	return c.doWith(c.hc, ctx, method, path, query, body, out)
}

func (c *Client) doWith(hc *http.Client, ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := c.base + "/api/v1" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return &ErrNotFound{URL: u}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("forgejo %s %s: %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// GetRepo returns repo metadata.
func (c *Client) GetRepo(ctx context.Context, org, repo string) (RepoInfo, error) {
	var raw map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo), nil, nil, &raw); err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{
		Org:           org,
		Repo:          repo,
		DefaultBranch: str(raw["default_branch"]),
		Private:       boolv(raw["private"]),
	}, nil
}

// RepoExists reports whether the repo exists.
func (c *Client) RepoExists(ctx context.Context, org, repo string) (bool, error) {
	_, err := c.GetRepo(ctx, org, repo)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*ErrNotFound); ok {
		return false, nil
	}
	return false, err
}

// EnsureOrg creates the organization if it does not exist.
func (c *Client) EnsureOrg(ctx context.Context, org string) error {
	var raw map[string]any
	err := c.do(ctx, "GET", "/orgs/"+seg(org), nil, nil, &raw)
	if err == nil {
		return nil
	}
	if _, ok := err.(*ErrNotFound); !ok {
		return err
	}
	return c.do(ctx, "POST", "/orgs", nil, map[string]any{"username": org, "visibility": "public"}, nil)
}

// EnsureRepo creates the repo (auto-init, default branch main) if absent and
// protects `main` (no direct pushes, even by admins), so main can only change
// by merging an MR.
func (c *Client) EnsureRepo(ctx context.Context, org, repo string) (bool, error) {
	created := false
	if ok, err := c.RepoExists(ctx, org, repo); err != nil {
		return false, err
	} else if !ok {
		if err := c.EnsureOrg(ctx, org); err != nil {
			return false, err
		}
		body := map[string]any{"name": repo, "auto_init": true, "default_branch": "main", "private": true}
		if err := c.do(ctx, "POST", "/orgs/"+seg(org)+"/repos", nil, body, nil); err != nil {
			// The owner may be a user, not an org; fall back to the user endpoint.
			if e := c.do(ctx, "POST", "/user/repos", nil, body, nil); e != nil {
				return false, err
			}
		}
		created = true
	}
	// Protect main unconditionally (idempotent: a 422 "already protected" is
	// tolerated).
	if err := c.ProtectMain(ctx, org, repo); err != nil {
		return created, err
	}
	return created, nil
}

// CreateEmptyRepo creates `org/repo` WITHOUT auto-init (no initial commit, no
// default branch) so a subsequent git PUSH establishes the branches. It is the
// destination step of an import: the external content is fetched by the
// gateway's git client and pushed, never by Forgejo's mirror-migrate (which
// would drag in every `refs/pull/*`). The org is created when absent.
func (c *Client) CreateEmptyRepo(ctx context.Context, org, repo, description string, private bool) (RepoInfo, error) {
	if err := c.EnsureOrg(ctx, org); err != nil {
		return RepoInfo{}, err
	}
	body := map[string]any{"name": repo, "auto_init": false, "private": private}
	if description != "" {
		body["description"] = description
	}
	if err := c.do(ctx, "POST", "/orgs/"+seg(org)+"/repos", nil, body, nil); err != nil {
		// The owner may be a user, not an org; fall back to the user endpoint.
		if e := c.do(ctx, "POST", "/user/repos", nil, body, nil); e != nil {
			return RepoInfo{}, err
		}
	}
	return c.GetRepo(ctx, org, repo)
}

// DeleteRepo removes a repository. Used to clean up a half-finished import
// (the repo was created but the push failed) so no empty orphan remains.
func (c *Client) DeleteRepo(ctx context.Context, org, repo string) error {
	err := c.do(ctx, "DELETE", "/repos/"+seg(org)+"/"+seg(repo), nil, nil, nil)
	if err != nil {
		if _, ok := err.(*ErrNotFound); ok {
			return nil
		}
	}
	return err
}

// SetDefaultBranch points a repo's default branch at `branch`.
func (c *Client) SetDefaultBranch(ctx context.Context, org, repo, branch string) error {
	return c.do(ctx, "PATCH", "/repos/"+seg(org)+"/"+seg(repo), nil, map[string]any{"default_branch": branch}, nil)
}

// ProtectMain makes `main` accept changes ONLY through an MR merge: direct
// pushes are disabled and the rule applies to admins too (so the shared token
// cannot bypass it).
func (c *Client) ProtectMain(ctx context.Context, org, repo string) error {
	body := map[string]any{
		"branch_name":                "main",
		"rule_name":                  "main",
		"enable_push":                false,
		"apply_to_admins":            true,
		"enable_approvals_whitelist": false,
	}
	err := c.do(ctx, "POST", "/repos/"+seg(org)+"/"+seg(repo)+"/branch_protections", nil, body, nil)
	if err != nil && strings.Contains(err.Error(), "already") {
		return nil
	}
	return err
}

// ---- container packages (OCI images) ----

// Package is one container image tag owned by an owner (user or org).
type Package struct {
	Owner string
	Name  string
	Tag   string
}

// ListContainerPackages lists an owner's container packages (paginated). When
// name is non-empty only that image's tags are returned.
func (c *Client) ListContainerPackages(ctx context.Context, owner, name string) ([]Package, error) {
	const pageSize = 50
	out := []Package{}
	for page := 1; page <= 100; page++ {
		q := url.Values{}
		q.Set("type", "container")
		q.Set("limit", fmt.Sprint(pageSize))
		q.Set("page", fmt.Sprint(page))
		var rows []map[string]any
		if err := c.do(ctx, "GET", "/packages/"+seg(owner), q, nil, &rows); err != nil {
			return nil, err
		}
		for _, r := range rows {
			p := Package{Owner: owner, Name: str(r["name"]), Tag: str(r["version"])}
			if name != "" && p.Name != name {
				continue
			}
			out = append(out, p)
		}
		if len(rows) < pageSize {
			break
		}
	}
	return out, nil
}

// ---- archive ----

// ArchiveTarGz downloads a repository tree at `ref` as a tar.gz stream.
func (c *Client) ArchiveTarGz(ctx context.Context, org, repo, ref string) ([]byte, error) {
	u := c.base + "/api/v1/repos/" + seg(org) + "/" + seg(repo) + "/archive/" + seg(ref+".tar.gz")
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, &ErrNotFound{URL: u}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("forgejo archive %s: %d: %s", ref, res.StatusCode, strings.TrimSpace(string(b)))
	}
	return io.ReadAll(res.Body)
}

// ---- branches ----

// BranchExists reports whether a branch exists.
func (c *Client) BranchExists(ctx context.Context, org, repo, branch string) (bool, error) {
	var raw map[string]any
	err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/branches/"+seg(branch), nil, nil, &raw)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*ErrNotFound); ok {
		return false, nil
	}
	return false, err
}

// CreateBranch creates `branch` from `from`. An "already exists" error is
// tolerated (idempotent), so a retry or a lifecycle-event race is harmless.
func (c *Client) CreateBranch(ctx context.Context, org, repo, branch, from string) error {
	body := map[string]any{"new_branch_name": branch, "old_ref_name": from}
	err := c.do(ctx, "POST", "/repos/"+seg(org)+"/"+seg(repo)+"/branches", nil, body, nil)
	if err != nil && isAlreadyExists(err) {
		return nil
	}
	return err
}

// DeleteBranch deletes a branch. A missing branch is tolerated.
func (c *Client) DeleteBranch(ctx context.Context, org, repo, branch string) error {
	err := c.do(ctx, "DELETE", "/repos/"+seg(org)+"/"+seg(repo)+"/branches/"+seg(branch), nil, nil, nil)
	if err != nil {
		if _, ok := err.(*ErrNotFound); ok {
			return nil
		}
		if isAlreadyExists(err) {
			return nil
		}
		// Forgejo answers a missing ref with 500 + "object does not exist"
		// (NOT 404), so treat that as an idempotent no-op too.
		if strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return nil
		}
	}
	return err
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") || strings.Contains(s, "409")
}

// ---- change requests ----

// MRInfo is one pull request.
type MRInfo struct {
	Index        int32  `json:"index"`
	Title        string `json:"title"`
	State        string `json:"state"`
	Head         string `json:"head"`
	Base         string `json:"base"`
	Body         string `json:"body"`
	Author       string `json:"author"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Merged       bool   `json:"merged"`
	Mergeable    bool   `json:"mergeable"`
	Additions    int32  `json:"additions"`
	Deletions    int32  `json:"deletions"`
	ChangedFiles int32  `json:"changed_files"`
	HTMLURL      string `json:"html_url"`
}

// MRComment is one issue/MR comment.
type MRComment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt string
	UpdatedAt string
}

// TagInfo is one repository tag.
type TagInfo struct {
	Name string
	SHA  string
}

// CommitDetail is one commit's metadata.
type CommitDetail struct {
	SHA         string
	Message     string
	Author      string
	AuthorEmail string
	Date        string
	Parents     []string
	HTMLURL     string
}

// CreateMR opens a pull request.
func (c *Client) CreateMR(ctx context.Context, org, repo, title, head, base, body string) (int32, string, error) {
	in := map[string]any{"title": title, "head": head, "base": base}
	if body != "" {
		in["body"] = body
	}
	var out map[string]any
	if err := c.do(ctx, "POST", "/repos/"+seg(org)+"/"+seg(repo)+"/pulls", nil, in, &out); err != nil {
		return 0, "", err
	}
	return int32(num(out["number"])), str(out["html_url"]), nil
}

// ListMRs lists pull requests (state "" = open).
func (c *Client) ListMRs(ctx context.Context, org, repo, state string) ([]MRInfo, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	q.Set("limit", "50")
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/pulls", q, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]MRInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, mrFromJSON(r))
	}
	return out, nil
}

// CommentMR posts a comment on a pull request.
func (c *Client) CommentMR(ctx context.Context, org, repo string, index int32, body string) error {
	return c.do(ctx, "POST", "/repos/"+seg(org)+"/"+seg(repo)+"/issues/"+fmt.Sprint(index)+"/comments", nil, map[string]any{"body": body}, nil)
}

// MergeMR merges a pull request (maintainer action; the only path to main).
func (c *Client) MergeMR(ctx context.Context, org, repo string, index int32) error {
	return c.do(ctx, "POST", "/repos/"+seg(org)+"/"+seg(repo)+"/pulls/"+fmt.Sprint(index)+"/merge", nil, map[string]any{}, nil)
}

// GetMR returns one pull request's detail (full MRInfo).
func (c *Client) GetMR(ctx context.Context, org, repo string, index int32) (MRInfo, error) {
	var r map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/pulls/"+fmt.Sprint(index), nil, nil, &r); err != nil {
		return MRInfo{}, err
	}
	return mrFromJSON(r), nil
}

// MRDiff returns the unified diff of a pull request (Forgejo `.diff`).
func (c *Client) MRDiff(ctx context.Context, org, repo string, index int32) (string, error) {
	return c.rawText("/repos/" + seg(org) + "/" + seg(repo) + "/pulls/" + fmt.Sprint(index) + ".diff")
}

// MRComments lists the issue comments (the MR conversation), oldest first.
func (c *Client) MRComments(ctx context.Context, org, repo string, index int32) ([]MRComment, error) {
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/issues/"+fmt.Sprint(index)+"/comments",
		url.Values{"limit": {"100"}}, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]MRComment, 0, len(rows))
	for _, r := range rows {
		user, _ := r["user"].(map[string]any)
		out = append(out, MRComment{
			ID:        int64(num(r["id"])),
			Author:    str(user["login"]),
			Body:      str(r["body"]),
			CreatedAt: str(r["created_at"]),
			UpdatedAt: str(r["updated_at"]),
		})
	}
	return out, nil
}

// Tags lists a repository's tags.
func (c *Client) Tags(ctx context.Context, org, repo string) ([]TagInfo, error) {
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/tags", url.Values{"limit": {"100"}}, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]TagInfo, 0, len(rows))
	for _, r := range rows {
		commit, _ := r["commit"].(map[string]any)
		out = append(out, TagInfo{Name: str(r["name"]), SHA: str(commit["sha"])})
	}
	return out, nil
}

// ---- releases ----

// ReleaseInfo is one Forgejo release with its assets.
type ReleaseInfo struct {
	ID          int64
	TagName     string
	Name        string
	Body        string
	Draft       bool
	Prerelease  bool
	Author      string
	CreatedAt   string
	PublishedAt string
	HTMLURL     string
	Assets      []ReleaseAsset
}

// ReleaseAsset is one uploaded file on a release.
type ReleaseAsset struct {
	ID            int64
	Name          string
	Size          int64
	DownloadCount int64
	DownloadURL   string
}

// ListReleases lists a repository's releases (newest first).
func (c *Client) ListReleases(ctx context.Context, org, repo string) ([]ReleaseInfo, error) {
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/releases",
		url.Values{"limit": {"50"}}, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]ReleaseInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, releaseFromJSON(r))
	}
	return out, nil
}

// GetReleaseAsset fetches one asset's bytes. The API returns the asset's
// METADATA (with a `browser_download_url`), so we resolve that URL and fetch
// the bytes from it. Forgejo nests assets under the release.
func (c *Client) GetReleaseAsset(ctx context.Context, org, repo string, releaseID, assetID int64) ([]byte, string, string, error) {
	var meta map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+
		"/releases/"+fmt.Sprint(releaseID)+"/assets/"+fmt.Sprint(assetID), nil, nil, &meta); err != nil {
		return nil, "", "", err
	}
	name := str(meta["name"])
	dl := str(meta["browser_download_url"])
	if dl == "" {
		return nil, "", "", fmt.Errorf("release asset %d has no download url", assetID)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", dl, nil)
	if err != nil {
		return nil, "", "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, "", "", &ErrNotFound{URL: dl}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, "", "", fmt.Errorf("forgejo asset download %d: %d: %s", assetID, res.StatusCode, strings.TrimSpace(string(b)))
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", "", err
	}
	return data, name, res.Header.Get("Content-Type"), nil
}

func releaseFromJSON(r map[string]any) ReleaseInfo {
	author, _ := r["author"].(map[string]any)
	out := ReleaseInfo{
		ID:          int64(num(r["id"])),
		TagName:     str(r["tag_name"]),
		Name:        str(r["name"]),
		Body:        str(r["body"]),
		Draft:       boolv(r["draft"]),
		Prerelease:  boolv(r["prerelease"]),
		Author:      str(author["login"]),
		CreatedAt:   str(r["created_at"]),
		PublishedAt: str(r["published_at"]),
		HTMLURL:     str(r["html_url"]),
	}
	if assets, ok := r["assets"].([]any); ok {
		for _, a := range assets {
			am, _ := a.(map[string]any)
			out.Assets = append(out.Assets, ReleaseAsset{
				ID:            int64(num(am["id"])),
				Name:          str(am["name"]),
				Size:          int64(num(am["size"])),
				DownloadCount: int64(num(am["download_count"])),
			})
		}
	}
	return out
}

// GetCommit returns one commit's metadata (first line of its message + author).
func (c *Client) GetCommit(ctx context.Context, org, repo, sha string) (CommitDetail, error) {
	var r map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/git/commits/"+seg(sha), nil, nil, &r); err != nil {
		return CommitDetail{}, err
	}
	commit, _ := r["commit"].(map[string]any)
	author, _ := commit["author"].(map[string]any)
	parents := []string{}
	if ps, ok := r["parents"].([]any); ok {
		for _, p := range ps {
			pm, _ := p.(map[string]any)
			if s := str(pm["sha"]); s != "" {
				parents = append(parents, s)
			}
		}
	}
	return CommitDetail{
		SHA:         str(r["sha"]),
		Message:     strings.TrimSpace(str(commit["message"])),
		Author:      str(author["name"]),
		AuthorEmail: str(author["email"]),
		Date:        str(author["date"]),
		Parents:     parents,
		HTMLURL:     str(r["html_url"]),
	}, nil
}

// CommitDiff returns the unified diff of one commit (Forgejo `.diff`).
func (c *Client) CommitDiff(ctx context.Context, org, repo, sha string) (string, error) {
	return c.rawText("/repos/" + seg(org) + "/" + seg(repo) + "/git/commits/" + seg(sha) + ".diff")
}

// RawFile reads a file's raw BYTES at ref/path (any content type).
func (c *Client) RawFile(ctx context.Context, org, repo, ref, path string) ([]byte, string, error) {
	u := c.base + "/api/v1/repos/" + seg(org) + "/" + seg(repo) + "/raw/" + encPath(path)
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, "", &ErrNotFound{URL: u}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, "", fmt.Errorf("forgejo raw %s@%s: %d: %s", path, ref, res.StatusCode, strings.TrimSpace(string(b)))
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, "", err
	}
	// The rev header carries the blob sha when available.
	return data, strings.TrimSpace(res.Header.Get("X-Git-SHA")), nil
}

// rawText GETs a Forgejo endpoint that returns text (e.g. a `.diff`).
func (c *Client) rawText(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v1"+path, nil)
	if err != nil {
		return "", err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return "", &ErrNotFound{URL: c.base + path}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("forgejo %s: %d: %s", path, res.StatusCode, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

// mrFromJSON maps a Forgejo pull-request object to MRInfo.
func mrFromJSON(r map[string]any) MRInfo {
	head, _ := r["head"].(map[string]any)
	base, _ := r["base"].(map[string]any)
	user, _ := r["user"].(map[string]any)
	return MRInfo{
		Index:        int32(num(r["number"])),
		Title:        str(r["title"]),
		State:        str(r["state"]),
		Head:         str(head["ref"]),
		Base:         str(base["ref"]),
		Body:         str(r["body"]),
		Author:       str(user["login"]),
		CreatedAt:    str(r["created_at"]),
		UpdatedAt:    str(r["updated_at"]),
		Merged:       boolv(r["merged"]),
		Mergeable:    boolv(r["mergeable"]),
		Additions:    int32(num(r["additions"])),
		Deletions:    int32(num(r["deletions"])),
		ChangedFiles: int32(num(r["changed_files"])),
		HTMLURL:      str(r["html_url"]),
	}
}

// Tree lists a ref's tree (recursive).
func (c *Client) Tree(ctx context.Context, org, repo, ref, path string) ([]TreeEntry, error) {
	if ref == "" {
		ref = "main"
	}
	q := url.Values{}
	if path != "" {
		q.Set("ref", ref)
		q.Set("path", path)
	}
	var raw any
	if path != "" {
		if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/contents/"+encPath(path), q, nil, &raw); err != nil {
			return nil, err
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("not a directory: %s", path)
		}
		out := make([]TreeEntry, 0, len(arr))
		for _, e := range arr {
			m, _ := e.(map[string]any)
			t := str(m["type"])
			if t == "dir" {
				t = "dir"
			} else {
				t = "file"
			}
			out = append(out, TreeEntry{Path: str(m["path"]), Type: t, Size: int64(num(m["size"]))})
		}
		return out, nil
	}
	// Root: use the git tree API (recursive) for a flat listing.
	var tree struct {
		Tree []map[string]any `json:"tree"`
	}
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/git/trees/"+url.PathEscape(ref), url.Values{"recursive": {"true"}}, nil, &tree); err != nil {
		return nil, err
	}
	out := make([]TreeEntry, 0, len(tree.Tree))
	for _, t := range tree.Tree {
		typ := "file"
		if str(t["type"]) == "tree" {
			typ = "dir"
		}
		out = append(out, TreeEntry{Path: str(t["path"]), Type: typ, Size: int64(num(t["size"]))})
	}
	return out, nil
}

// ReadBlob reads a UTF-8 file at ref/path.
func (c *Client) ReadBlob(ctx context.Context, org, repo, ref, path string) (string, string, error) {
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	var raw map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/contents/"+encPath(path), q, nil, &raw); err != nil {
		return "", "", err
	}
	content := str(raw["content"])
	if str(raw["encoding"]) == "base64" {
		if b, err := base64.StdEncoding.DecodeString(content); err == nil {
			content = string(b)
		}
	}
	return content, str(raw["sha"]), nil
}

// Log lists commits (optionally path-scoped).
func (c *Client) Log(ctx context.Context, org, repo, ref, path string, limit int) ([]CommitInfo, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprint(limit))
	if ref != "" {
		q.Set("sha", ref)
	}
	if path != "" {
		q.Set("path", path)
	}
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/commits", q, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]CommitInfo, 0, len(rows))
	for _, r := range rows {
		commit, _ := r["commit"].(map[string]any)
		author, _ := commit["author"].(map[string]any)
		out = append(out, CommitInfo{
			SHA:     str(r["sha"]),
			Message: strings.TrimSpace(str(commit["message"])),
			Author:  str(author["name"]),
			Date:    str(author["date"]),
		})
	}
	return out, nil
}

// Branches lists branches.
func (c *Client) Branches(ctx context.Context, org, repo string) ([]BranchInfo, error) {
	var rows []map[string]any
	if err := c.do(ctx, "GET", "/repos/"+seg(org)+"/"+seg(repo)+"/branches", nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]BranchInfo, 0, len(rows))
	for _, r := range rows {
		commit, _ := r["commit"].(map[string]any)
		out = append(out, BranchInfo{Name: str(r["name"]), SHA: str(commit["id"])})
	}
	return out, nil
}

// ListRepos lists every repo the shared credential can see (used to seed the
// members table / browse). Forgejo has no org listing scoped to a token
// without admin, so we use the search endpoint.
func (c *Client) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	var res struct {
		Data []map[string]any `json:"data"`
	}
	if err := c.do(ctx, "GET", "/repos/search", url.Values{"limit": {"50"}}, nil, &res); err != nil {
		return nil, err
	}
	out := make([]RepoInfo, 0, len(res.Data))
	for _, r := range res.Data {
		owner, _ := r["owner"].(map[string]any)
		out = append(out, RepoInfo{
			Org:           str(owner["login"]),
			Repo:          str(r["name"]),
			DefaultBranch: str(r["default_branch"]),
			Private:       boolv(r["private"]),
		})
	}
	return out, nil
}

func seg(s string) string { return url.PathEscape(s) }

func encPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	n, _ := v.(float64)
	return n
}

func boolv(v any) bool {
	b, _ := v.(bool)
	return b
}
