package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// fakeAgent records tenant-scoped provider registrations and config sets.
type fakeAgent struct {
	mu        sync.Mutex
	tenants   []string
	providers []string // "<tenant>|Authorization"
	configs   []string // "<tenant>|<extId>.<name>=<value>"
	deleted   []string // provider ids deleted
}

func (f *fakeAgent) start(t *testing.T) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		body := string(buf[:n])
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "AdminService/ListTenants"):
			ids := make([]string, 0, len(f.tenants))
			for _, id := range f.tenants {
				ids = append(ids, `{"id":"`+id+`"}`)
			}
			_, _ = w.Write([]byte(`{"tenants":[` + strings.Join(ids, ",") + `]}`))
		case strings.HasSuffix(r.URL.Path, "AdminService/IssueTenantToken"):
			// token == "tok-<tenant>"
			var req struct {
				TenantID string `json:"tenantId"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			_, _ = w.Write([]byte(`{"plaintext":"tok-` + req.TenantID + `"}`))
		case strings.HasSuffix(r.URL.Path, "AgentService/RegisterProvider"):
			var req struct {
				Provider struct {
					ProviderID string `json:"providerId"`
					BaseURL    string `json:"baseUrl"`
				} `json:"provider"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			f.providers = append(f.providers, req.Provider.ProviderID+"|"+auth+"|"+req.Provider.BaseURL)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "AgentService/DeleteProvider"):
			var req struct {
				ProviderID string `json:"providerId"`
			}
			_ = json.Unmarshal([]byte(body), &req)
			f.deleted = append(f.deleted, req.ProviderID)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "AgentService/SetExtensionConfig"):
			f.configs = append(f.configs, auth+"|"+body)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	return httptest.NewServer(h2c.NewHandler(handler, &http2.Server{}))
}

func TestRunSeedsProvidersPerProfile(t *testing.T) {
	fa := &fakeAgent{tenants: []string{"workspace", "myuser"}}
	srv := fa.start(t)
	defer srv.Close()

	res, err := Run(context.Background(), Config{
		AgentURL:       srv.URL,
		AdminToken:     "admin",
		DefaultProfile: "std",
		// A stale `myuser: full` override must NOT strand the tenant: the run
		// warns and falls back to the default profile.
		TenantProfile: map[string]string{"myuser": "full"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// workspace + myuser each get the (7-provider) gray profile.
	if res.Providers != 14 {
		t.Fatalf("providers = %d, want 14 (2 tenants x 7)", res.Providers)
	}
	// BOTH tenants must be on the gray gateway.
	for _, tenant := range []string{"workspace", "myuser"} {
		want := "gateway-text|Bearer tok-" + tenant + "|"
		found := false
		for _, p := range fa.providers {
			if strings.HasPrefix(p, want) && strings.Contains(p, grayBase) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s must use the gray (std) profile", tenant)
		}
	}
	// NO registered provider may carry a `local/` model.
	if strings.Contains(strings.Join(fa.providers, ";"), "local/") {
		t.Fatalf("no local/* models may be registered: %v", fa.providers)
	}
	// The retired override produced a warning (not a hard skip).
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for the unknown `full` profile")
	}
}

func TestRunDeletesRetiredProviders(t *testing.T) {
	fa := &fakeAgent{tenants: []string{"workspace"}}
	srv := fa.start(t)
	defer srv.Close()

	res, err := Run(context.Background(), Config{
		AgentURL:         srv.URL,
		AdminToken:       "admin",
		DefaultProfile:   "std",
		RetiredProviders: []string{"gateway-video"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProvidersRemoved != 1 {
		t.Fatalf("providers_removed = %d, want 1", res.ProvidersRemoved)
	}
	if len(fa.deleted) != 1 || fa.deleted[0] != "gateway-video" {
		t.Fatalf("deleted = %v, want [gateway-video]", fa.deleted)
	}
}

func TestRunCalibrationExpandsStar(t *testing.T) {
	fa := &fakeAgent{tenants: []string{"workspace", "myuser"}}
	srv := fa.start(t)
	defer srv.Close()

	res, err := Run(context.Background(), Config{
		AgentURL:       srv.URL,
		AdminToken:     "admin",
		DefaultProfile: "std",
		Calibrations:   []Calibration{{Tenant: "*", ExtID: "workspace", Name: "forgejo-token", Value: "PAT"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// One calibration per tenant.
	if res.Calibrations != 2 {
		t.Fatalf("calibrations = %d, want 2", res.Calibrations)
	}
	for _, c := range fa.configs {
		if !strings.Contains(c, `"forgejo-token"`) || !strings.Contains(c, `"PAT"`) {
			t.Fatalf("calibration body missing value: %s", c)
		}
	}
}
