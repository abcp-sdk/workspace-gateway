package workspacesvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/internal/imagebuild"
	"github.com/abcp-sdk/workspace-gateway/internal/roles"
)

// buildState is one background image build's progress.
type buildState struct {
	state    string // running | done | failed
	imageRef string
	log      string
}

// builds tracks background image builds by id. The map is process-local (a
// build is bound to the replica that started it); entries are small and a build
// is short-lived relative to the process. NOTE: with >1 replica and no sticky
// routing, a poll may hit another replica and get NotFound; single-replica or
// sticky sessions are required for reliable polling.
var (
	buildsMu sync.Mutex
	builds   = map[string]*buildState{}
)

func newBuildID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// BuildSandboxImage starts a background build and returns its id IMMEDIATELY
// (the build's push is the slow part). Poll GetBuildStatus for the result.
func (s *Service) BuildSandboxImage(ctx context.Context, req *connect.Request[wsv1.BuildSandboxImageRequest]) (*connect.Response[wsv1.BuildSandboxImageResponse], error) {
	if _, err := s.sandboxAuth(ctx, req.Header()); err != nil {
		return nil, err
	}
	// Validate everything up front (fail fast, before returning a build id),
	// and fetch the repo archive. The slow push happens in the goroutine below.
	breq, cleanup, err := s.prepareBuild(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	id := newBuildID()
	buildsMu.Lock()
	builds[id] = &buildState{state: "running"}
	buildsMu.Unlock()
	// Detach from the request context so the build survives the RPC returning.
	go func() {
		defer cleanup()
		res, err := s.builder.Build(context.WithoutCancel(ctx), breq)
		buildsMu.Lock()
		defer buildsMu.Unlock()
		st := builds[id]
		if st == nil {
			return
		}
		if err != nil {
			st.state, st.log = "failed", err.Error()
			return
		}
		st.state, st.imageRef, st.log = "done", res.ImageRef, res.Log
	}()
	return connect.NewResponse(&wsv1.BuildSandboxImageResponse{BuildId: id}), nil
}

// GetBuildStatus returns a background build's state.
func (s *Service) GetBuildStatus(ctx context.Context, req *connect.Request[wsv1.GetBuildStatusRequest]) (*connect.Response[wsv1.GetBuildStatusResponse], error) {
	if _, err := s.sandboxAuth(ctx, req.Header()); err != nil {
		return nil, err
	}
	id := req.Msg.GetBuildId()
	buildsMu.Lock()
	st := builds[id]
	buildsMu.Unlock()
	if st == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("build %q not found", id))
	}
	return connect.NewResponse(&wsv1.GetBuildStatusResponse{
		BuildId: id, State: st.state, ImageRef: st.imageRef, Log: st.log,
	}), nil
}

// prepareBuild validates a build request and extracts the repo archive into a
// build context dir, returning the builder request. It does NOT run the build
// (that is backgrounded in BuildSandboxImage). The caller must call cleanup.
func (s *Service) prepareBuild(ctx context.Context, m *wsv1.BuildSandboxImageRequest) (imagebuild.Request, func(), error) {
	org, repo, ref := m.GetOrg(), m.GetRepo(), m.GetRef()
	if !roles.ValidComponent(org) || !roles.ValidComponent(repo) {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("org/repo must be simple names"))
	}
	if ref == "" {
		ref = roles.MainBranch
	}
	if !roles.ValidComponent(ref) {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("ref must be a simple name"))
	}
	if !imageNameRe.MatchString(m.GetImage()) {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("image must be a single simple name"))
	}
	if !roles.ValidComponent(m.GetTag()) {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tag must be a simple name"))
	}
	if s.builder == nil {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeUnavailable, errors.New("image builder not configured"))
	}
	archive, err := s.git.ArchiveTarGz(ctx, org, repo, ref)
	if err != nil {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInternal, fmt.Errorf("fetch repo archive: %w", err))
	}
	dir, cleanup, err := imagebuild.Extract(archive, 0)
	if err != nil {
		return imagebuild.Request{}, nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// The image is pushed under the SOURCE REPO's org namespace
	// (<registry>/<org>/<image>:<tag>).
	return imagebuild.Request{
		ContextDir: dir,
		Dockerfile: m.GetDockerfile(),
		Context:    m.GetContext(),
		Repo:       org + "/" + m.GetImage(),
		Tag:        m.GetTag(),
		BuildArgs:  s.buildArgs(m.GetBuildArgs()),
	}, cleanup, nil
}
