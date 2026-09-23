// Package workerclient is a minimal agent-worker (worker.v1) Connect client over
// plain HTTP/1.1. The gateway uses it only to observe a sandbox's job history
// (for idle reclamation); execution tools live in the workspace extension.
package workerclient

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	workerv1 "github.com/abcp-sdk/workspace-gateway/gen/worker/v1"
	"github.com/abcp-sdk/workspace-gateway/gen/worker/v1/workerv1connect"
)

// Job is a worker job summary.
type Job struct {
	ID         string
	Command    string
	State      string
	ExitCode   int32
	StartedAt  int64 // unix millis
	FinishedAt int64 // unix millis, 0 while running
}

// JobOutput is a window of one job's buffered output lines.
type JobOutput struct {
	Lines      []string
	TotalLines int32
	StartLine  int32
	EndLine    int32
	Done       bool
}

// WatchEvent is one message of a live job watch: either an incremental output
// chunk or the terminal event (Done=true, with the exit code + tails).
type WatchEvent struct {
	Output   string
	Done     bool
	ExitCode int32
	Stdout   string
	Stderr   string
}

// Info is the worker environment summary.
type Info struct {
	OS        string
	Arch      string
	Workspace string // absolute workspace root (relative paths resolve here)
	Home      string // the worker user's home dir (the OS `~`)
}

// FileEntry is one entry of a directory listing. Path is workspace-relative
// when inside the workspace, absolute otherwise.
type FileEntry struct {
	Path  string
	Size  int64
	IsDir bool
}

// FileList is a directory listing.
type FileList struct {
	IsDir bool
	Files []FileEntry
}

// FileRead is a (windowed) file read.
type FileRead struct {
	Content    []byte
	TotalLines int32
	StartLine  int32
	EndLine    int32
}

// Client talks to one agent-worker endpoint.
type Client struct {
	c workerv1connect.WorkerServiceClient
}

// New builds a bearer-authenticated client for url.
func New(url, token string) *Client {
	opts := []connect.ClientOption{}
	if token != "" {
		opts = append(opts, connect.WithInterceptors(bearer(token)))
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	return &Client{c: workerv1connect.NewWorkerServiceClient(hc, strings.TrimRight(url, "/"), opts...)}
}

// ListJobs returns the worker's job history window (≤500 entries, 24h retained).
func (c *Client) ListJobs(ctx context.Context) ([]Job, error) {
	res, err := c.c.ListJobs(ctx, connect.NewRequest(&workerv1.ListJobsRequest{}))
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(res.Msg.GetJobs()))
	for _, j := range res.Msg.GetJobs() {
		out = append(out, Job{
			ID: j.GetId(), Command: j.GetCommand(), State: j.GetState(),
			ExitCode: j.GetExitCode(), StartedAt: j.GetStartedAt(), FinishedAt: j.GetFinishedAt(),
		})
	}
	return out, nil
}

// Info returns the worker's environment summary.
func (c *Client) Info(ctx context.Context) (Info, error) {
	res, err := c.c.Info(ctx, connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		return Info{}, err
	}
	m := res.Msg
	return Info{OS: m.GetOs(), Arch: m.GetArch(), Workspace: m.GetWorkspace(), Home: m.GetHome()}, nil
}

// FileList lists a directory (or a single file) on the worker.
func (c *Client) FileList(ctx context.Context, path string, depth, limit int32) (FileList, error) {
	res, err := c.c.FileList(ctx, connect.NewRequest(&workerv1.FileListRequest{
		Path: path, Depth: depth, Limit: limit,
	}))
	if err != nil {
		return FileList{}, err
	}
	m := res.Msg
	out := FileList{IsDir: m.GetIsDir(), Files: make([]FileEntry, 0, len(m.GetFiles()))}
	for _, f := range m.GetFiles() {
		out.Files = append(out.Files, FileEntry{Path: f.GetPath(), Size: f.GetSize(), IsDir: f.GetIsDir()})
	}
	return out, nil
}

// FileRead reads a (windowed) file from the worker.
func (c *Client) FileRead(ctx context.Context, path string, start, end int32) (FileRead, error) {
	res, err := c.c.FileRead(ctx, connect.NewRequest(&workerv1.FileReadRequest{
		Path: path, StartLine: start, EndLine: end,
	}))
	if err != nil {
		return FileRead{}, err
	}
	m := res.Msg
	return FileRead{
		Content: m.GetContent(), TotalLines: m.GetTotalLines(),
		StartLine: m.GetStartLine(), EndLine: m.GetEndLine(),
	}, nil
}

// JobOutput polls a bounded window of one job's buffered output.
func (c *Client) JobOutput(ctx context.Context, jobID string, start, end int32, stream string) (JobOutput, error) {
	res, err := c.c.JobOutput(ctx, connect.NewRequest(&workerv1.JobOutputRequest{
		JobId: jobID, Start: start, End: end, Stream: stream,
	}))
	if err != nil {
		return JobOutput{}, err
	}
	m := res.Msg
	return JobOutput{
		Lines: m.GetLines(), TotalLines: m.GetTotalLines(),
		StartLine: m.GetStartLine(), EndLine: m.GetEndLine(), Done: m.GetDone(),
	}, nil
}

// WatchJob streams a job's output: buffered history first, then live chunks,
// ending with the terminal event. The caller consumes until the channel closes.
func (c *Client) WatchJob(ctx context.Context, jobID string) (<-chan WatchEvent, <-chan error) {
	events := make(chan WatchEvent, 64)
	errc := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errc)
		st, err := c.c.WatchJob(ctx, connect.NewRequest(&workerv1.WatchJobRequest{JobId: jobID}))
		if err != nil {
			errc <- err
			return
		}
		defer st.Close()
		for st.Receive() {
			m := st.Msg()
			if d := m.GetDone(); d != nil {
				events <- WatchEvent{Done: true, ExitCode: d.GetExitCode(), Stdout: d.GetStdout(), Stderr: d.GetStderr()}
				return
			}
			if out := m.GetOutput(); out != "" {
				events <- WatchEvent{Output: out}
			}
		}
		if err := st.Err(); err != nil {
			errc <- err
		}
	}()
	return events, errc
}

func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}
