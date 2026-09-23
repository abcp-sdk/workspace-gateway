package internal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/abcp-sdk/agent-worker/gen/worker/v1"
	"github.com/abcp-sdk/agent-worker/internal/filesvc"
	"github.com/abcp-sdk/agent-worker/internal/jobsvc"
	"github.com/abcp-sdk/agent-worker/internal/shellh"
)

// jobWaitMax caps JobWait. Configurable via WORKER_WAIT_MAX (go duration or
// bare seconds); the 600s default lets the common "wait for the build" call
// block in ONE rpc instead of client-side polling slices.
var jobWaitMax = waitMaxFromEnv()

func waitMaxFromEnv() time.Duration {
	d, err := time.ParseDuration(os.Getenv("WORKER_WAIT_MAX"))
	if err != nil || d <= 0 {
		return 600 * time.Second
	}
	return d
}

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
		Os:           goos(),
		Arch:         goarch(),
		Shell:        "builtin(mvdan-sh)",
		Workspace:    s.files.Root(),
		BootId:       s.bootID,
		DroppedLines: s.jobs.Dropped(),
	}), nil
}

func (s *WorkerService) Execute(ctx context.Context, req *connect.Request[workerv1.ExecuteRequest]) (*connect.Response[workerv1.ExecuteResponse], error) {
	if req.Msg.Command == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("command required"))
	}
	id, err := s.jobs.Execute(ctx, req.Msg.Command, req.Msg.Workdir, req.Msg.Env, req.Msg.TimeoutMs)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&workerv1.ExecuteResponse{JobId: id}), nil
}

func (s *WorkerService) ListJobs(ctx context.Context, req *connect.Request[workerv1.ListJobsRequest]) (*connect.Response[workerv1.ListJobsResponse], error) {
	limit := int(req.Msg.Limit)
	var out []*workerv1.JobEntry
	for _, r := range s.jobs.List(limit) {
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
		stdout := s.jobs.ReplayTailStream(req.Msg.JobId, jobsvc.StreamStdout, doneTailLines)
		stderr := s.jobs.ReplayTailStream(req.Msg.JobId, jobsvc.StreamStderr, doneTailLines)
		return stream.Send(&workerv1.WatchJobResponse{Event: &workerv1.WatchJobResponse_Done_{Done: &workerv1.WatchJobResponse_Done{
			ExitCode: row.ExitCode,
			Stdout:   joinLines(stdout),
			Stderr:   joinLines(stderr),
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

// doneTailLines is the tail size carried in the terminal Done event.
const doneTailLines = 50

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
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
	// BLOCKING wait — no poll loop: the goroutine parks until the job
	// completes, the caller disconnects or the deadline fires.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-job.Done():
		shellh.Debugf("JobWait rpc: job %s -> done branch", req.Msg.JobId)
		res := job.Result()
		return connect.NewResponse(&workerv1.JobWaitResponse{State: job.State, ExitCode: res.ExitCode}), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		shellh.Debugf("JobWait rpc: job %s -> TIMEOUT branch (state=%s)", req.Msg.JobId, job.State)
		return connect.NewResponse(&workerv1.JobWaitResponse{State: job.State, ExitCode: job.ExitCode}), nil
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
	if req.Msg.StartLine == 0 && req.Msg.EndLine == 0 {
		// Whole file (backward-compatible no-window call).
		data, err := s.files.Read(req.Msg.Path)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewResponse(&workerv1.FileReadResponse{Content: data}), nil
	}
	data, total, start, end, err := s.files.ReadWindow(req.Msg.Path, req.Msg.StartLine, req.Msg.EndLine)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.FileReadResponse{
		Content:    data,
		TotalLines: total,
		StartLine:  start,
		EndLine:    end,
	}), nil
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

func (s *WorkerService) FileDelete(ctx context.Context, req *connect.Request[workerv1.FileDeleteRequest]) (*connect.Response[workerv1.FileDeleteResponse], error) {
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("path required"))
	}
	if err := s.files.Delete(req.Msg.Path); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.FileDeleteResponse{Ok: true}), nil
}

func (s *WorkerService) FileMove(ctx context.Context, req *connect.Request[workerv1.FileMoveRequest]) (*connect.Response[workerv1.FileMoveResponse], error) {
	if req.Msg.From == "" || req.Msg.To == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from and to required"))
	}
	if err := s.files.Move(req.Msg.From, req.Msg.To); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.FileMoveResponse{Ok: true}), nil
}

func (s *WorkerService) FileCopy(ctx context.Context, req *connect.Request[workerv1.FileCopyRequest]) (*connect.Response[workerv1.FileCopyResponse], error) {
	if req.Msg.From == "" || req.Msg.To == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("from and to required"))
	}
	if err := s.files.Copy(req.Msg.From, req.Msg.To); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&workerv1.FileCopyResponse{Ok: true}), nil
}

func (s *WorkerService) FileList(ctx context.Context, req *connect.Request[workerv1.FileListRequest]) (*connect.Response[workerv1.FileListResponse], error) {
	isDir, entries, _, err := s.files.List(req.Msg.Path, int(req.Msg.Depth), int(req.Msg.Limit))
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
