package forgejo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureRepoProtectsMain(t *testing.T) {
	var protected bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/repos/acme/app"):
			w.WriteHeader(404)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/orgs/acme"):
			w.WriteHeader(404)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/orgs"):
			w.WriteHeader(201)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/orgs/acme/repos"):
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"name":"app","owner":{"login":"acme"},"default_branch":"main"}`))
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/branch_protections"):
			protected = true
			if !strings.Contains(readBody(r), `"apply_to_admins":true`) {
				t.Errorf("branch protection must apply to admins: %s", readBody(r))
			}
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	created, err := c.EnsureRepo(context.Background(), "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected created=true")
	}
	if !protected {
		t.Fatal("main was not protected")
	}
}

func TestMergeMRPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if err := c.MergeMR(context.Background(), "acme", "app", 7); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/repos/acme/app/pulls/7/merge" {
		t.Fatalf("wrong merge path: %s", path)
	}
}

func TestListRepos(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"name":"app","owner":{"login":"acme"},"default_branch":"main","private":true}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	repos, err := c.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Org != "acme" || repos[0].Repo != "app" || !repos[0].Private {
		t.Fatalf("bad repos: %+v", repos)
	}
}

func TestListContainerPackages(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"name":"toolchain-node","version":"debian-trixie"},
			{"name":"toolchain-go","version":"debian-trixie"}
		]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")

	all, err := c.ListContainerPackages(context.Background(), "agent-toolchain", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "toolchain-node" || all[0].Owner != "agent-toolchain" {
		t.Fatalf("bad packages: %+v", all)
	}
	if gotPath != "/api/v1/packages/agent-toolchain" || !strings.Contains(gotQuery, "type=container") {
		t.Fatalf("bad request: %s?%s", gotPath, gotQuery)
	}

	// name filters to one image.
	one, err := c.ListContainerPackages(context.Background(), "agent-toolchain", "toolchain-go")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Name != "toolchain-go" {
		t.Fatalf("filter failed: %+v", one)
	}
}

func readBody(r *http.Request) string {
	b := make([]byte, 4096)
	n, _ := r.Body.Read(b)
	return string(b[:n])
}

func TestMergeMRConflictMapsToErrConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"merge failed because of conflict"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	err := c.MergeMR(context.Background(), "acme", "app", 1)
	var conflict *ErrConflict
	if !errorsAs(err, &conflict) {
		t.Fatalf("expected *ErrConflict, got %T (%v)", err, err)
	}
	if !strings.Contains(conflict.Reason, "conflict") {
		t.Fatalf("reason not surfaced: %q", conflict.Reason)
	}
}

func TestMergeMRSendsMergeStyle(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = readBody(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if err := c.MergeMR(context.Background(), "acme", "app", 3); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"Do":"merge"`) {
		t.Fatalf("merge must send Do=merge, got %s", body)
	}
}

func TestCompareFilesParsesChangedPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/compare/main...feature") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"files":[{"filename":"a.txt"},{"filename":"dir/b.go"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	paths, err := c.CompareFiles(context.Background(), "acme", "app", "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "a.txt" || paths[1] != "dir/b.go" {
		t.Fatalf("paths = %v", paths)
	}
}

// errorsAs is a tiny wrapper so the test stays dependency-light.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
