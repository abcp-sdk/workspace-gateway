package internal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/easylab-platform/easyworker/gen/worker/v1"
	"github.com/easylab-platform/easyworker/internal/filesvc"
	"github.com/easylab-platform/easyworker/internal/jobsvc"
	"github.com/easylab-platform/easyworker/internal/shellh"
)

// jobWaitMax caps JobWait (legacy contract: 60s; ext-ops reads with a 65s
// budget). NB: must be a typed time.Duration — an untyped 60_000 would
// compare as 60µs (ns units) and clamp every wait to a hair trigger.
const jobWaitMax = 60 * time.Second

// WorkerService implements workerv1connect.WorkerServiceHandler.
type WorkerService struct {
	jobs   *jobsvc.Manager
	files  *filesvc.Service
	shell  *shellh.Runner
	bootID string
}

func NewService(jobs *jobsvc.Manager, files *filesvc.Service, shell *shellh.Runner) *WorkerService {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return &WorkerService{jobs: jobs, files: files, shell: shell, bootID: hex.EncodeToString(b[:])}
}

func (s *WorkerService) Info(ctx context.Context, req *connect.Request[workerv1.InfoRequest]) (*connect.Response[workerv1.InfoResponse], error) {
	return connect.NewResponse(&workerv1.InfoResponse{
		Os:        goos(),
		Arch:      goarch(),
		Shell:     "builtin(mvdan-sh)",
		Workspace: s.files.Root(),
		BootId:    s.bootID,
	}), nil
}

func (s *WorkerService) Execute(ctx context.Context, req *connect.Request[workerv1.ExecuteRequest]) (*connect.Response[workerv1.ExecuteResponse], error) {
	if req.Msg.Command == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("command required"))
	}
	id, err := s.jobs.Execute(ctx, req.Msg.Command, req.Msg.Workdir, req.Msg.Env)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&workerv1.ExecuteResponse{JobId: id}), nil
}

func (s *WorkerService) ListJobs(ctx context.Context, req *connect.Request[workerv1.ListJobsRequest]) (*connect.Response[workerv1.ListJobsResponse], error) {
	var out []*workerv1.JobEntry
	for _, r := range s.jobs.List(500) {
		out = append(out, &workerv1.JobEntry{
			Id:         r.ID,
			Command:    r.Command,
			State:      r.State,
			ExitCode:   r.ExitCode,
			StartedAt:  r.StartedAt,
			FinishedAt: r.FinishedAt,
		})
	}
	return connect.NewResponse(&workerv1.ListJobsResponse{Jobs: out}), nil
}

// WatchJob replays persisted+live history as output events, then streams
// live output until completion (terminal Done event always sent last).
func (s *WorkerService) WatchJob(ctx context.Context, req *connect.Request[workerv1.WatchJobRequest], stream *connect.ServerStream[workerv1.WatchJobResponse]) error {
	if req.Msg.JobId == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("job_id required"))
	}
	job, memErr := s.jobs.Get(req.Msg.JobId)

	// Finished long ago (not in memory): replay from the store, send Done.
	if memErr != nil {
		row, ok, err := s.jobs.Store().QueryJob(req.Msg.JobId)
		if err != nil || !ok {
			return connect.NewError(connect.CodeNotFound, errors.New("job not found"))
		}
		for _, line := range s.jobs.ReplayTail(req.Msg.JobId, replayCap) {
			if err := stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Output{Output: line}}); err != nil {
				return err
			}
		}
		stdout := tailOf(s.jobs, req.Msg.JobId, jobsvc.StreamStdout)
		stderr := tailOf(s.jobs, req.Msg.JobId, jobsvc.StreamStderr)
		return stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Done_{Done: &workerv1.WatchJobResponse_Done{
			ExitCode: row.ExitCode,
			Stdout:   stdout,
			Stderr:   stderr,
		}}})
	}

	// Live path: replay recent history, subscribe, stream until Done.
	replay, live := job.Subscribe()
	for _, line := range replay {
		if err := stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Output{Output: line}}); err != nil {
			return err
		}
	}
	if live != nil {
		for line := range live {
			if err := stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Output{Output: line}}); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	res := job.Result()
	return stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Done_{Done: &workerv1.WatchJobResponse_Done{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
	}}})
}

// replayCap bounds WatchJob's replay from history (live window is separate).
const replayCap = 1000

func tailOf(m *jobsvc.Manager, id string, stream int) string {
	recs, err := m.MergedRecs(id, stream)
	if err != nil {
		return ""
	}
	n := 50
	if len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	out := ""
	for i, r := range recs {
		if i > 0 {
			out += "\n"
		}
		out += r.Line
	}
	return out
}

func (s *WorkerService) JobOutput(ctx context.Context, req *connect.Request[workerv1.JobOutputRequest]) (*connect.Response[workerv1.JobOutputResponse], error) {
	if req.Msg.JobId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("job_id required"))
	}
	stream := streamOf(req.Msg.Stream)
	lines, total, start, end, done, err := s.jobs.OutputLines(req.Msg.JobId, stream, req.Msg.Start, req.Msg.End)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.JobOutputResponse{
		Lines:      lines,
		TotalLines: total,
		StartLine:  start,
		EndLine:    end,
		Done:       done,
	}), nil
}

func streamOf(s string) int {
	switch s {
	case "stdout":
		return jobsvc.StreamStdout
	case "stderr":
		return jobsvc.StreamStderr
	default:
		return -1 // all
	}
}

func (s *WorkerService) JobWait(ctx context.Context, req *connect.Request[workerv1.JobWaitRequest]) (*connect.Response[workerv1.JobWaitResponse], error) {
	shellh.Debugf("JobWait rpc: job %s timeoutMs=%d mono=%d", req.Msg.JobId, req.Msg.TimeoutMs, shellh.MonoMS())
	job, err := s.jobs.Get(req.Msg.JobId)
	if err != nil {
		// Post-restart finished job: answer from the store.
		row, ok, serr := s.jobs.Store().QueryJob(req.Msg.JobId)
		if serr != nil || !ok {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("job not found"))
		}
		return connect.NewResponse(&workerv1.JobWaitResponse{State: row.State, ExitCode: row.ExitCode}), nil
	}
	timeout := time.Duration(req.Msg.TimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > jobWaitMax {
		timeout = jobWaitMax
	}
	// Poll loop with a done-first check: the happy path (job already
	// finished) answers instantly, and the deadline is enforced by
	// iteration count rather than a single long timer.
	const tick = 25 * time.Millisecond
	iters := int(timeout / tick)
	if iters < 1 {
		iters = 1
	}
	for i := 0; ; i++ {
		select {
		case <-job.Done():
			shellh.Debugf("JobWait rpc: job %s -> done branch", req.Msg.JobId)
			res := job.Result()
			return connect.NewResponse(&workerv1.JobWaitResponse{State: job.State, ExitCode: res.ExitCode}), nil
		default:
		}
		if i >= iters {
			shellh.Debugf("JobWait rpc: job %s -> TIMEOUT branch (state=%s)", req.Msg.JobId, job.State)
			return connect.NewResponse(&workerv1.JobWaitResponse{State: job.State, ExitCode: job.ExitCode}), nil
		}
		select {
		case <-job.Done():
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(tick):
		}
	}
}

func (s *WorkerService) JobStdin(ctx context.Context, req *connect.Request[workerv1.JobStdinRequest]) (*connect.Response[workerv1.JobStdinResponse], error) {
	job, err := s.jobs.Get(req.Msg.JobId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err := job.Stdin(req.Msg.Data, req.Msg.Close); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&workerv1.JobStdinResponse{Ok: true}), nil
}

func (s *WorkerService) JobKill(ctx context.Context, req *connect.Request[workerv1.JobKillRequest]) (*connect.Response[workerv1.JobKillResponse], error) {
	shellh.Debugf("JobKill rpc: job %s", req.Msg.JobId)
	job, err := s.jobs.Get(req.Msg.JobId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	job.Kill()
	shellh.Debugf("JobKill rpc: cancel() called for %s mono=%d", req.Msg.JobId, shellh.MonoMS())
	return connect.NewResponse(&workerv1.JobKillResponse{Ok: true}), nil
}

func (s *WorkerService) FileRead(ctx context.Context, req *connect.Request[workerv1.FileReadRequest]) (*connect.Response[workerv1.FileReadResponse], error) {
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path required"))
	}
	data, err := s.files.Read(req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.FileReadResponse{Content: data}), nil
}

func (s *WorkerService) FileWrite(ctx context.Context, req *connect.Request[workerv1.FileWriteRequest]) (*connect.Response[workerv1.FileWriteResponse], error) {
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path required"))
	}
	if err := s.files.Write(req.Msg.Path, req.Msg.Content); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&workerv1.FileWriteResponse{Ok: true}), nil
}

func (s *WorkerService) FileList(ctx context.Context, req *connect.Request[workerv1.FileListRequest]) (*connect.Response[workerv1.FileListResponse], error) {
	isDir, entries, err := s.files.List(req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var out []*workerv1.FileEntry
	for _, e := range entries {
		out = append(out, &workerv1.FileEntry{Path: e.Path, Size: e.Size, IsDir: e.IsDir})
	}
	return connect.NewResponse(&workerv1.FileListResponse{IsDir: isDir, Files: out}), nil
}

// SyncFolder unpacks a directory-tree tarball into the workspace (the
// easylab -> worker checkout path for sandbox/CI workspaces).
func (s *WorkerService) SyncFolder(ctx context.Context, req *connect.Request[workerv1.SyncFolderRequest]) (*connect.Response[workerv1.SyncFolderResponse], error) {
	n, err := s.files.SyncFolder(req.Msg.Tarball, req.Msg.Dest, req.Msg.Clean)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&workerv1.SyncFolderResponse{Files: int32(n), Root: s.files.Root()}), nil
}

// BootID exposes this worker process's boot id (surfaced by Info; used as the
// enrollment gate's identity).
func (s *WorkerService) BootID() string { return s.bootID }
