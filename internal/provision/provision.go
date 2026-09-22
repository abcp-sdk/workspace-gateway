// Package provision brings a freshly-booted workspace agent up to the
// deployment's expected state, IDEMPOTENTLY:
//
//   - PROVIDERS: the workspace agent starts with none. Sessions need a
//     `provider_id/model_id` to run, so every tenant must be seeded with the
//     platform gateway providers. There are TWO profiles, mirroring the
//     standalone agent:
//     myuser -> the dev004 gateway (key Hzfsls...) whose model set includes the
//     `local/*` extras; every OTHER tenant -> the gray gateway (key
//     sk-code...) without `local/*`.
//
//   - CALIBRATION: the agent seeds extension config with CREATE-IF-ABSENT, so a
//     value written early (or migrated from an older deployment) silently wins
//     over the chart's authoritative value forever. Calibration FORCE-SETS the
//     deployment-owned knobs (gateway-url/token, forgejo-url/token, …) so a
//     stale value cannot strand a tenant.
//
// It talks to the agent over h2c as the ADMIN (to mint a per-tenant token),
// then acts AS each tenant for RegisterProvider / SetExtensionConfig.
package provision

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/agent/v1/agentv1connect"
)

// Model is one provider model entry.
type Model struct {
	ID   string
	Name string
	// ContextLimit is >0 for text models and 0 for every other modality.
	ContextLimit int64
}

// Provider is one modality-scoped provider.
type Provider struct {
	ID         string
	Capability string
	Models     []Model
}

// Profile is a gateway endpoint + key + the providers it serves.
type Profile struct {
	BaseURL   string
	APIKey    string
	Providers []Provider
}

// Calibration force-sets one tenant's global extension config value.
type Calibration struct {
	Tenant string `json:"tenant"`
	ExtID  string `json:"extId"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// Config configures a provisioning run.
type Config struct {
	AgentURL   string
	AdminToken string
	// Profiles by name; the built-ins are ProfileStd and ProfileFull.
	Profiles map[string]Profile
	// TenantProfile overrides the default profile for a named tenant.
	TenantProfile map[string]string
	// DefaultProfile is used for any tenant without an override.
	DefaultProfile string
	// Calibrations are force-set on their (resolved) tenants.
	Calibrations []Calibration
	// Timeout bounds the whole run. Zero = 5 minutes.
	Timeout time.Duration
}

// Result summarizes a run.
type Result struct {
	Tenants      []string
	Providers    int
	Calibrations int
	// Warnings are non-fatal (a single provider/knob that failed). The run
	// keeps going so one bad tenant does not block the rest.
	Warnings []string
}

// Gateway credentials for the two platform gateways. Kept here (not in chart
// values) because they are dev fixtures already tracked in tools/rebind-gateway.py.
const (
	grayBase = "https://api-gray.xueersi.com/ai-multimodal-gateway/v4/ai"
	grayKey  = "sk-code-dAeG7zpfYuusoIA0Z9LYumE6oDb7BNArqxTkk4th37lYtzRokkkvoY5Y"

	dev004Base = "https://ai-gateway-dev004.develop.fenjin.org/v4/ai"
	dev004Key  = "Hzfsls978665#"

	textCtx = int64(262144)

	apiType = "vercel-compatible-gateway"
)

// sharedProviders is the model set every tenant gets (the gray gateway's
// visibility: no `local/*` models).
func sharedProviders() []Provider {
	return []Provider{
		{ID: "gateway-text", Capability: "text", Models: []Model{
			{"tal-coding/deepseek-v4.1-flash", "DeepSeek V4.1 Flash", textCtx},
			{"tal-coding/glm-5.3", "GLM 5.3", textCtx},
			{"tal-coding/glm-5.3-flash", "GLM 5.3 Flash", textCtx},
		}},
		{ID: "gateway-image", Capability: "image", Models: []Model{
			{"tal-coding-gptimage/gpt-image-2", "GPT Image 2", 0},
			{"tal-coding-gptimage/gpt-image-2.5-flare", "GPT Image 2.5 Flare", 0},
			{"tal-coding-gptimage/gpt-image-2.5-sunburst", "GPT Image 2.5 Sunburst", 0},
		}},
		{ID: "gateway-video", Capability: "video", Models: []Model{
			{"local/minimax-h3-video", "MiniMax H3 Video", 0},
		}},
		{ID: "gateway-speech", Capability: "speech", Models: []Model{
			{"mlops-tts-customvoice/qwen3-tts-customvoice", "Qwen3 TTS CustomVoice", 0},
			{"mlops-tts-base/qwen3-tts-base", "Qwen3 TTS Base", 0},
		}},
		{ID: "gateway-transcription", Capability: "transcription", Models: []Model{
			{"mlops-asr/qwen3-asr", "Qwen3 ASR", 0},
		}},
		{ID: "gateway-embedding", Capability: "embedding", Models: []Model{
			{"mlops-embed/gte-multilingual-base", "GTE Multilingual", 0},
		}},
		{ID: "gateway-rerank", Capability: "rerank", Models: []Model{
			{"mlops-rerank/gte-multilingual-reranker-base", "GTE Reranker", 0},
		}},
		{ID: "gateway-realtime", Capability: "realtime", Models: []Model{
			{"mlops-voxtral/voxtral-realtime", "Voxtral Realtime", 0},
		}},
	}
}

// ProfileStd is the default deployment profile (the gray gateway).
var ProfileStd = Profile{BaseURL: grayBase, APIKey: grayKey, Providers: sharedProviders()}

// ProfileFull is myuser's profile (the dev004 gateway + the local/* extras).
var ProfileFull = func() Profile {
	ps := sharedProviders()
	for i := range ps {
		switch ps[i].ID {
		case "gateway-text":
			ps[i].Models = append(ps[i].Models, Model{"local/local-text", "Local Text", textCtx})
		case "gateway-image":
			ps[i].Models = append(ps[i].Models,
				Model{"local/local-image", "Local Image", 0},
				Model{"local/local-image-edit", "Local Image Edit", 0})
		case "gateway-video":
			ps[i].Models = append(ps[i].Models, Model{"local/local-video", "Local Video", 0})
		}
	}
	return Profile{BaseURL: dev004Base, APIKey: dev004Key, Providers: ps}
}()

// BuiltinProfiles returns a fresh map of the two built-in profiles.
func BuiltinProfiles() map[string]Profile {
	return map[string]Profile{"std": ProfileStd, "full": ProfileFull}
}

// Run provisions the agent. It is safe to run repeatedly.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.AgentURL == "" {
		return Result{}, errors.New("agent url is required")
	}
	if cfg.AdminToken == "" {
		return Result{}, errors.New("admin token is required")
	}
	if len(cfg.Profiles) == 0 {
		cfg.Profiles = BuiltinProfiles()
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = "std"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c := newClient(cfg.AgentURL)
	adminReq := connect.NewRequest(&agentv1.ListTenantsRequest{})
	adminReq.Header().Set("Authorization", "Bearer "+cfg.AdminToken)
	tenants, err := c.admin.ListTenants(ctx, adminReq)
	if err != nil {
		return Result{}, fmt.Errorf("ListTenants: %w", err)
	}

	res := Result{}
	for _, t := range tenants.Msg.GetTenants() {
		tenant := t.GetId()
		if tenant == "" || t.GetDisabled() {
			continue
		}
		res.Tenants = append(res.Tenants, tenant)

		token, err := mintToken(ctx, c, cfg.AdminToken, tenant)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("tenant %s: mint token: %v", tenant, err))
			continue
		}

		profileName := cfg.DefaultProfile
		if p, ok := cfg.TenantProfile[tenant]; ok {
			profileName = p
		}
		profile, ok := cfg.Profiles[profileName]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("tenant %s: unknown profile %q", tenant, profileName))
			continue
		}
		for _, p := range profile.Providers {
			if err := registerProvider(ctx, c, token, profile, p); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("tenant %s: register %s: %v", tenant, p.ID, err))
				continue
			}
			res.Providers++
		}
	}

	for _, cal := range cfg.Calibrations {
		for _, tenant := range resolveTenants(cal.Tenant, res.Tenants) {
			token, err := mintToken(ctx, c, cfg.AdminToken, tenant)
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("calibrate %s/%s.%s: mint token: %v", tenant, cal.ExtID, cal.Name, err))
				continue
			}
			if err := setConfig(ctx, c, token, cal); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("calibrate %s.%s.%s: %v", tenant, cal.ExtID, cal.Name, err))
				continue
			}
			res.Calibrations++
		}
	}

	return res, nil
}

// resolveTenants expands the pseudo-tenant `*` to every known tenant.
func resolveTenants(tenant string, known []string) []string {
	if tenant == "*" {
		return known
	}
	return []string{tenant}
}

func mintToken(ctx context.Context, c *client, adminToken, tenant string) (string, error) {
	req := connect.NewRequest(&agentv1.IssueTenantTokenRequest{TenantId: tenant, Label: "provision"})
	req.Header().Set("Authorization", "Bearer "+adminToken)
	res, err := c.admin.IssueTenantToken(ctx, req)
	if err != nil {
		return "", err
	}
	if res.Msg.GetPlaintext() == "" {
		return "", errors.New("empty tenant token")
	}
	return res.Msg.GetPlaintext(), nil
}

func registerProvider(ctx context.Context, c *client, token string, profile Profile, p Provider) error {
	models := make([]*agentv1.ProviderModel, 0, len(p.Models))
	for _, m := range p.Models {
		models = append(models, &agentv1.ProviderModel{
			Id: m.ID, Name: m.Name, ContextLimit: m.ContextLimit, ModelType: p.Capability,
		})
	}
	req := connect.NewRequest(&agentv1.RegisterProviderRequest{Provider: &agentv1.Provider{
		ProviderId: p.ID,
		ApiType:    apiType,
		BaseUrl:    profile.BaseURL,
		ApiKey:     profile.APIKey,
		Models:     models,
		Capability: p.Capability,
	}})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := c.agent.RegisterProvider(ctx, req)
	return err
}

func setConfig(ctx context.Context, c *client, token string, cal Calibration) error {
	value, err := structpb.NewValue(cal.Value)
	if err != nil {
		return err
	}
	req := connect.NewRequest(&agentv1.SetExtensionConfigRequest{
		ExtId: cal.ExtID,
		Name:  cal.Name,
		Value: value,
	})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err = c.agent.SetExtensionConfig(ctx, req)
	return err
}

// client is a small h2c Connect client exposing both services.
type client struct {
	agent agentv1connect.AgentServiceClient
	admin agentv1connect.AdminServiceClient
}

func newClient(url string) *client {
	p := new(http.Protocols)
	p.SetHTTP1(false)
	p.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{Protocols: p}, Timeout: 30 * time.Second}
	base := trimSlash(url)
	// JSON codec: the agent serves it, and it keeps this tool's wire format
	// inspectable (it is an operational tool, not a hot path).
	opts := []connect.ClientOption{connect.WithProtoJSON()}
	return &client{
		agent: agentv1connect.NewAgentServiceClient(hc, base, opts...),
		admin: agentv1connect.NewAdminServiceClient(hc, base, opts...),
	}
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
