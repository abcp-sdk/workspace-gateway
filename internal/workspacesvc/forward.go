package workspacesvc

import (
	"context"
	"errors"
	"io"

	"connectrpc.com/connect/v2"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	wsv1connect "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1/wsv1connect"
)

// This file forwards the minimal agent surface the webui needs. The webui
// never speaks agent.v1; it goes through these methods. In connect v2 request
// metadata lives on the context's CallInfo (messages carry no headers), so each
// forward call re-seeds the outbound client context from the inbound server
// context — the tenant bearer reaches the agent verbatim.

// fwdCtx returns a client context whose request headers carry the caller's
// (server-side) request headers, so the forwarded agent call authenticates the
// real tenant.
func fwdCtx(ctx context.Context) context.Context {
	src, ok := connect.CallInfoForServerContext(ctx)
	if !ok {
		return ctx
	}
	out, dst := connect.NewClientContext(ctx)
	for k, vs := range src.RequestHeader().All() {
		if hopHeaders[k] {
			continue
		}
		for _, v := range vs {
			dst.RequestHeader().Add(k, v)
		}
	}
	return out
}

func (s *Service) Health(ctx context.Context, r *agentv1.HealthRequest) (*agentv1.HealthResponse, error) {
	return s.agent.Health(fwdCtx(ctx), r)
}

func (s *Service) GetIdentity(ctx context.Context, r *agentv1.GetIdentityRequest) (*agentv1.GetIdentityResponse, error) {
	return s.agent.GetIdentity(fwdCtx(ctx), r)
}

func (s *Service) ListSessions(ctx context.Context, r *agentv1.ListSessionsRequest) (*agentv1.ListSessionsResponse, error) {
	return s.agent.ListSessions(fwdCtx(ctx), r)
}

func (s *Service) GetSession(ctx context.Context, r *agentv1.GetSessionRequest) (*agentv1.GetSessionResponse, error) {
	return s.agent.GetSession(fwdCtx(ctx), r)
}

func (s *Service) ListMessages(ctx context.Context, r *agentv1.ListMessagesRequest) (*agentv1.ListMessagesResponse, error) {
	return s.agent.ListMessages(fwdCtx(ctx), r)
}

func (s *Service) Prompt(ctx context.Context, r *agentv1.PromptRequest, st wsv1connect.BranchSessionServicePromptServerStream) error {
	up, err := s.agent.Prompt(fwdCtx(ctx), r)
	if err != nil {
		return err
	}
	defer up.Close()
	for {
		msg, err := up.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := st.Send(msg); err != nil {
			return err
		}
	}
}

func (s *Service) WatchSession(ctx context.Context, r *agentv1.WatchSessionRequest, st wsv1connect.BranchSessionServiceWatchSessionServerStream) error {
	up, err := s.agent.WatchSession(fwdCtx(ctx), r)
	if err != nil {
		return err
	}
	defer up.Close()
	for {
		msg, err := up.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := st.Send(msg); err != nil {
			return err
		}
	}
}

func (s *Service) WatchSessions(ctx context.Context, r *agentv1.WatchSessionsRequest, st wsv1connect.BranchSessionServiceWatchSessionsServerStream) error {
	up, err := s.agent.WatchSessions(fwdCtx(ctx), r)
	if err != nil {
		return err
	}
	defer up.Close()
	for {
		msg, err := up.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := st.Send(msg); err != nil {
			return err
		}
	}
}

func (s *Service) SetModel(ctx context.Context, r *agentv1.SetModelRequest) (*agentv1.SetModelResponse, error) {
	return s.agent.SetModel(fwdCtx(ctx), r)
}

func (s *Service) Undo(ctx context.Context, r *agentv1.UndoRequest) (*agentv1.UndoResponse, error) {
	return s.agent.Undo(fwdCtx(ctx), r)
}

func (s *Service) MarkRead(ctx context.Context, r *agentv1.MarkReadRequest) (*agentv1.MarkReadResponse, error) {
	return s.agent.MarkRead(fwdCtx(ctx), r)
}

func (s *Service) State(ctx context.Context, r *agentv1.StateRequest) (*agentv1.StateResponse, error) {
	return s.agent.State(fwdCtx(ctx), r)
}

func (s *Service) Mailbox(ctx context.Context, r *agentv1.MailboxRequest) (*agentv1.MailboxResponse, error) {
	return s.agent.Mailbox(fwdCtx(ctx), r)
}

func (s *Service) Interrupt(ctx context.Context, r *agentv1.InterruptRequest) (*agentv1.InterruptResponse, error) {
	return s.agent.Interrupt(fwdCtx(ctx), r)
}

func (s *Service) Compact(ctx context.Context, r *agentv1.CompactRequest) (*agentv1.CompactResponse, error) {
	return s.agent.Compact(fwdCtx(ctx), r)
}

// UpdateSettings is NARROWED: it forwards only model/variant/locale. The
// policy-bearing fields (preset/max_turns/system_prompt/group) are not part of
// this surface at all, so a client cannot forge them.
func (s *Service) UpdateSettings(ctx context.Context, r *wsv1.UpdateSettingsRequest) (*wsv1.UpdateSettingsResponse, error) {
	m := r
	out := &agentv1.UpdateSettingsRequest{
		Id:      m.GetId(),
		Model:   m.GetModel(),
		Variant: m.GetVariant(),
		Locale:  m.GetLocale(),
	}
	res, err := s.agent.UpdateSettings(fwdCtx(ctx), out)
	if err != nil {
		return nil, err
	}
	return &wsv1.UpdateSettingsResponse{Session: res.GetSession()}, nil
}

func (s *Service) ListProviders(ctx context.Context, r *agentv1.ListProvidersRequest) (*agentv1.ListProvidersResponse, error) {
	return s.agent.ListProviders(fwdCtx(ctx), r)
}

func (s *Service) ListProvidersCatalog(ctx context.Context, r *agentv1.ListProvidersCatalogRequest) (*agentv1.ListProvidersCatalogResponse, error) {
	return s.agent.ListProvidersCatalog(fwdCtx(ctx), r)
}

func (s *Service) RegisterProvider(ctx context.Context, r *agentv1.RegisterProviderRequest) (*agentv1.RegisterProviderResponse, error) {
	return s.agent.RegisterProvider(fwdCtx(ctx), r)
}

func (s *Service) DeleteProvider(ctx context.Context, r *agentv1.DeleteProviderRequest) (*agentv1.DeleteProviderResponse, error) {
	return s.agent.DeleteProvider(fwdCtx(ctx), r)
}

func (s *Service) TestProvider(ctx context.Context, r *agentv1.TestProviderRequest) (*agentv1.TestProviderResponse, error) {
	return s.agent.TestProvider(fwdCtx(ctx), r)
}

func (s *Service) ListModels(ctx context.Context, r *agentv1.ListModelsRequest) (*agentv1.ListModelsResponse, error) {
	return s.agent.ListModels(fwdCtx(ctx), r)
}

func (s *Service) ListPresets(ctx context.Context, r *agentv1.ListPresetsRequest) (*agentv1.ListPresetsResponse, error) {
	return s.agent.ListPresets(fwdCtx(ctx), r)
}

func (s *Service) GetConfig(ctx context.Context, r *agentv1.GetConfigRequest) (*agentv1.GetConfigResponse, error) {
	return s.agent.GetConfig(fwdCtx(ctx), r)
}

func (s *Service) SetConfig(ctx context.Context, r *agentv1.SetConfigRequest) (*agentv1.SetConfigResponse, error) {
	return s.agent.SetConfig(fwdCtx(ctx), r)
}

func (s *Service) ListTools(ctx context.Context, r *agentv1.ListToolsRequest) (*agentv1.ListToolsResponse, error) {
	return s.agent.ListTools(fwdCtx(ctx), r)
}

func (s *Service) GetToolConfig(ctx context.Context, r *agentv1.GetToolConfigRequest) (*agentv1.GetToolConfigResponse, error) {
	return s.agent.GetToolConfig(fwdCtx(ctx), r)
}

func (s *Service) SetToolConfig(ctx context.Context, r *agentv1.SetToolConfigRequest) (*agentv1.SetToolConfigResponse, error) {
	return s.agent.SetToolConfig(fwdCtx(ctx), r)
}

func (s *Service) SetExtensionConfig(ctx context.Context, r *agentv1.SetExtensionConfigRequest) (*agentv1.SetExtensionConfigResponse, error) {
	return s.agent.SetExtensionConfig(fwdCtx(ctx), r)
}

func (s *Service) UploadFile(ctx context.Context, r *agentv1.UploadFileRequest) (*agentv1.UploadFileResponse, error) {
	return s.agent.UploadFile(fwdCtx(ctx), r)
}

func (s *Service) IngestFile(ctx context.Context, r *agentv1.IngestFileRequest) (*agentv1.IngestFileResponse, error) {
	return s.agent.IngestFile(fwdCtx(ctx), r)
}

func (s *Service) GetFile(ctx context.Context, r *agentv1.GetFileRequest) (*agentv1.GetFileResponse, error) {
	return s.agent.GetFile(fwdCtx(ctx), r)
}

func (s *Service) GetFileMeta(ctx context.Context, r *agentv1.GetFileMetaRequest) (*agentv1.GetFileMetaResponse, error) {
	return s.agent.GetFileMeta(fwdCtx(ctx), r)
}

func (s *Service) GetFileStream(ctx context.Context, r *agentv1.GetFileRequest, st wsv1connect.BranchSessionServiceGetFileStreamServerStream) error {
	up, err := s.agent.GetFileStream(fwdCtx(ctx), r)
	if err != nil {
		return err
	}
	defer up.Close()
	for {
		msg, err := up.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := st.Send(msg); err != nil {
			return err
		}
	}
}

var _ wsv1connect.BranchSessionServiceHandler = (*Service)(nil)

// fwdClientInterceptor forwards the inbound (server-side) request headers onto
// the outbound agent client call. In connect v2 request metadata lives on the
// context's CallInfo, so this is the header-forwarding analogue of the v1
// per-message header copy.
func fwdClientInterceptor() connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			src, ok := connect.CallInfoForServerContext(ctx)
			if !ok {
				return next(ctx, spec)
			}
			out, dst := connect.NewClientContext(ctx)
			for _, name := range forwardedHeaders {
				if v := src.RequestHeader().Get(name); v != "" {
					dst.RequestHeader().Set(name, v)
				}
			}
			return next(out, spec)
		}
	}
}

// forwardedHeaders are the caller headers the gateway relays to the agent so it
// authenticates the real tenant and derives the right public URLs.
var forwardedHeaders = []string{"Authorization", "X-Abc-Tenant", "X-Session-Name", "X-Forwarded-Host", "Host"}
