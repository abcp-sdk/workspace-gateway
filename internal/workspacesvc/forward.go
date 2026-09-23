package workspacesvc

import (
	"context"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/workspace-gateway/gen/agent/v1"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
)

// This file forwards the minimal agent surface the webui needs. The webui
// never speaks agent.v1; it goes through these methods. Headers (the tenant
// bearer) are copied verbatim so the agent authenticates the real caller.

func fwd[I any](in *connect.Request[I]) *connect.Request[I] {
	out := connect.NewRequest(in.Msg)
	copyHeaders(out, in.Header())
	return out
}

func (s *Service) Health(ctx context.Context, r *connect.Request[agentv1.HealthRequest]) (*connect.Response[agentv1.HealthResponse], error) {
	return s.agent.Health(ctx, fwd(r))
}

func (s *Service) GetIdentity(ctx context.Context, r *connect.Request[agentv1.GetIdentityRequest]) (*connect.Response[agentv1.GetIdentityResponse], error) {
	return s.agent.GetIdentity(ctx, fwd(r))
}

func (s *Service) ListSessions(ctx context.Context, r *connect.Request[agentv1.ListSessionsRequest]) (*connect.Response[agentv1.ListSessionsResponse], error) {
	return s.agent.ListSessions(ctx, fwd(r))
}

func (s *Service) GetSession(ctx context.Context, r *connect.Request[agentv1.GetSessionRequest]) (*connect.Response[agentv1.GetSessionResponse], error) {
	return s.agent.GetSession(ctx, fwd(r))
}

func (s *Service) ListMessages(ctx context.Context, r *connect.Request[agentv1.ListMessagesRequest]) (*connect.Response[agentv1.ListMessagesResponse], error) {
	return s.agent.ListMessages(ctx, fwd(r))
}

func (s *Service) Prompt(ctx context.Context, r *connect.Request[agentv1.PromptRequest], st *connect.ServerStream[agentv1.PromptResponse]) error {
	up, err := s.agent.Prompt(ctx, fwd(r))
	if err != nil {
		return err
	}
	return forwardStream(up, st)
}

func (s *Service) WatchSession(ctx context.Context, r *connect.Request[agentv1.WatchSessionRequest], st *connect.ServerStream[agentv1.WatchSessionResponse]) error {
	up, err := s.agent.WatchSession(ctx, fwd(r))
	if err != nil {
		return err
	}
	return forwardStream(up, st)
}

func (s *Service) WatchSessions(ctx context.Context, r *connect.Request[agentv1.WatchSessionsRequest], st *connect.ServerStream[agentv1.WatchSessionsResponse]) error {
	up, err := s.agent.WatchSessions(ctx, fwd(r))
	if err != nil {
		return err
	}
	return forwardStream(up, st)
}

func (s *Service) SetModel(ctx context.Context, r *connect.Request[agentv1.SetModelRequest]) (*connect.Response[agentv1.SetModelResponse], error) {
	return s.agent.SetModel(ctx, fwd(r))
}

func (s *Service) Undo(ctx context.Context, r *connect.Request[agentv1.UndoRequest]) (*connect.Response[agentv1.UndoResponse], error) {
	return s.agent.Undo(ctx, fwd(r))
}

func (s *Service) State(ctx context.Context, r *connect.Request[agentv1.StateRequest]) (*connect.Response[agentv1.StateResponse], error) {
	return s.agent.State(ctx, fwd(r))
}

func (s *Service) Mailbox(ctx context.Context, r *connect.Request[agentv1.MailboxRequest]) (*connect.Response[agentv1.MailboxResponse], error) {
	return s.agent.Mailbox(ctx, fwd(r))
}

func (s *Service) Interrupt(ctx context.Context, r *connect.Request[agentv1.InterruptRequest]) (*connect.Response[agentv1.InterruptResponse], error) {
	return s.agent.Interrupt(ctx, fwd(r))
}

func (s *Service) Compact(ctx context.Context, r *connect.Request[agentv1.CompactRequest]) (*connect.Response[agentv1.CompactResponse], error) {
	return s.agent.Compact(ctx, fwd(r))
}

// UpdateSettings is NARROWED: it forwards only model/variant/locale. The
// policy-bearing fields (preset/max_turns/system_prompt/group) are not part of
// this surface at all, so a client cannot forge them.
func (s *Service) UpdateSettings(ctx context.Context, r *connect.Request[wsv1.UpdateSettingsRequest]) (*connect.Response[wsv1.UpdateSettingsResponse], error) {
	m := r.Msg
	out := connect.NewRequest(&agentv1.UpdateSettingsRequest{
		Id:      m.GetId(),
		Model:   m.GetModel(),
		Variant: m.GetVariant(),
		Locale:  m.GetLocale(),
	})
	copyHeaders(out, r.Header())
	res, err := s.agent.UpdateSettings(ctx, out)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&wsv1.UpdateSettingsResponse{Session: res.Msg.GetSession()}), nil
}

func (s *Service) ListProviders(ctx context.Context, r *connect.Request[agentv1.ListProvidersRequest]) (*connect.Response[agentv1.ListProvidersResponse], error) {
	return s.agent.ListProviders(ctx, fwd(r))
}

func (s *Service) ListProvidersCatalog(ctx context.Context, r *connect.Request[agentv1.ListProvidersCatalogRequest]) (*connect.Response[agentv1.ListProvidersCatalogResponse], error) {
	return s.agent.ListProvidersCatalog(ctx, fwd(r))
}

func (s *Service) RegisterProvider(ctx context.Context, r *connect.Request[agentv1.RegisterProviderRequest]) (*connect.Response[agentv1.RegisterProviderResponse], error) {
	return s.agent.RegisterProvider(ctx, fwd(r))
}

func (s *Service) DeleteProvider(ctx context.Context, r *connect.Request[agentv1.DeleteProviderRequest]) (*connect.Response[agentv1.DeleteProviderResponse], error) {
	return s.agent.DeleteProvider(ctx, fwd(r))
}

func (s *Service) TestProvider(ctx context.Context, r *connect.Request[agentv1.TestProviderRequest]) (*connect.Response[agentv1.TestProviderResponse], error) {
	return s.agent.TestProvider(ctx, fwd(r))
}

func (s *Service) ListModels(ctx context.Context, r *connect.Request[agentv1.ListModelsRequest]) (*connect.Response[agentv1.ListModelsResponse], error) {
	return s.agent.ListModels(ctx, fwd(r))
}

func (s *Service) ListPresets(ctx context.Context, r *connect.Request[agentv1.ListPresetsRequest]) (*connect.Response[agentv1.ListPresetsResponse], error) {
	return s.agent.ListPresets(ctx, fwd(r))
}

func (s *Service) PreviewPreset(ctx context.Context, r *connect.Request[agentv1.PreviewPresetRequest]) (*connect.Response[agentv1.PreviewPresetResponse], error) {
	return s.agent.PreviewPreset(ctx, fwd(r))
}

func (s *Service) GetConfig(ctx context.Context, r *connect.Request[agentv1.GetConfigRequest]) (*connect.Response[agentv1.GetConfigResponse], error) {
	return s.agent.GetConfig(ctx, fwd(r))
}

func (s *Service) SetConfig(ctx context.Context, r *connect.Request[agentv1.SetConfigRequest]) (*connect.Response[agentv1.SetConfigResponse], error) {
	return s.agent.SetConfig(ctx, fwd(r))
}

func (s *Service) ListTools(ctx context.Context, r *connect.Request[agentv1.ListToolsRequest]) (*connect.Response[agentv1.ListToolsResponse], error) {
	return s.agent.ListTools(ctx, fwd(r))
}

func (s *Service) GetToolConfig(ctx context.Context, r *connect.Request[agentv1.GetToolConfigRequest]) (*connect.Response[agentv1.GetToolConfigResponse], error) {
	return s.agent.GetToolConfig(ctx, fwd(r))
}

func (s *Service) SetToolConfig(ctx context.Context, r *connect.Request[agentv1.SetToolConfigRequest]) (*connect.Response[agentv1.SetToolConfigResponse], error) {
	return s.agent.SetToolConfig(ctx, fwd(r))
}

func (s *Service) SetExtensionConfig(ctx context.Context, r *connect.Request[agentv1.SetExtensionConfigRequest]) (*connect.Response[agentv1.SetExtensionConfigResponse], error) {
	return s.agent.SetExtensionConfig(ctx, fwd(r))
}

func (s *Service) UploadFile(ctx context.Context, r *connect.Request[agentv1.UploadFileRequest]) (*connect.Response[agentv1.UploadFileResponse], error) {
	return s.agent.UploadFile(ctx, fwd(r))
}

func (s *Service) IngestFile(ctx context.Context, r *connect.Request[agentv1.IngestFileRequest]) (*connect.Response[agentv1.IngestFileResponse], error) {
	return s.agent.IngestFile(ctx, fwd(r))
}

func (s *Service) GetFile(ctx context.Context, r *connect.Request[agentv1.GetFileRequest]) (*connect.Response[agentv1.GetFileResponse], error) {
	return s.agent.GetFile(ctx, fwd(r))
}

func (s *Service) GetFileMeta(ctx context.Context, r *connect.Request[agentv1.GetFileMetaRequest]) (*connect.Response[agentv1.GetFileMetaResponse], error) {
	return s.agent.GetFileMeta(ctx, fwd(r))
}

func (s *Service) GetFileStream(ctx context.Context, r *connect.Request[agentv1.GetFileRequest], st *connect.ServerStream[agentv1.FileChunk]) error {
	up, err := s.agent.GetFileStream(ctx, fwd(r))
	if err != nil {
		return err
	}
	return forwardStream(up, st)
}

type stream[T any] interface {
	Receive() bool
	Msg() *T
	Err() error
	Close() error
}

func forwardStream[T any](up stream[T], down *connect.ServerStream[T]) error {
	defer up.Close()
	for up.Receive() {
		if err := down.Send(up.Msg()); err != nil {
			return err
		}
	}
	return up.Err()
}
