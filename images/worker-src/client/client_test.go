package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	workerv1 "github.com/easylab-platform/easyworker/gen/worker/v1"
	"github.com/easylab-platform/easyworker/gen/worker/v1/workerv1connect"
	"github.com/easylab-platform/easyworker/internal"
	"github.com/easylab-platform/easyworker/internal/auth"
	"github.com/easylab-platform/easyworker/internal/filesvc"
	"github.com/easylab-platform/easyworker/internal/jobsvc"
	"github.com/easylab-platform/easyworker/internal/shellh"
)

// newServer builds a real worker handler (auth gate + enroll + WorkerService)
// on an httptest server, returning it plus the gate that owns the code.
func newServer(t *testing.T) (*httptest.Server, *auth.Gate) {
	t.Helper()
	ws := t.TempDir()
	runner := shellh.New(ws, nil)
	store, err := jobsvc.OpenStore(t.TempDir() + "/jobs.db")
	if err != nil {
		t.Fatal(err)
	}
	jobs := jobsvc.NewManager(runner, store, 100, 100)
	files := filesvc.New(ws)
	svc := internal.NewService(jobs, files, runner)
	gate, err := auth.New(auth.Options{BootID: "boot-1"})
	if err != nil {
		t.Fatal(err)
	}
	enroll := internal.NewEnrollService(gate)

	mux := http.NewServeMux()
	mux.Handle(workerv1connect.NewWorkerServiceHandler(svc, connect.WithInterceptors(auth.NewInterceptor(gate))))
	mux.Handle(workerv1connect.NewWorkerEnrollHandler(enroll))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, gate
}

func TestEnrollThenDial(t *testing.T) {
	srv, gate := newServer(t)
	ctx := context.Background()

	// Status is open while unclaimed.
	st, err := Status(ctx, srv.URL)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.NeedsCode || st.Claimed {
		t.Fatalf("status = %+v", st)
	}

	// Unauthenticated WorkerService use is refused.
	if _, err := Dial(srv.URL, "").Info(ctx, connect.NewRequest(&workerv1.InfoRequest{})); err == nil {
		t.Fatal("unauthenticated Info must fail")
	}

	// Claim, then the issued token works.
	tok, err := Enroll(ctx, srv.URL, gate.Code(), "svc-a")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := Dial(srv.URL, tok).Info(ctx, connect.NewRequest(&workerv1.InfoRequest{})); err != nil {
		t.Fatalf("authed Info failed: %v", err)
	}

	// A second caller cannot take over.
	if _, err := Enroll(ctx, srv.URL, "whatever", "svc-b"); err != ErrAlreadyClaimed {
		t.Fatalf("second enroll err = %v, want ErrAlreadyClaimed", err)
	}
}

// TestStreamingCarriesBearer guards the regression where a unary-only bearer
// interceptor left server-streaming RPCs unauthenticated (WatchJob 401).
func TestStreamingCarriesBearer(t *testing.T) {
	srv, gate := newServer(t)
	ctx := context.Background()
	tok, err := Enroll(ctx, srv.URL, gate.Code(), "svc")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	cli := Dial(srv.URL, tok)

	// Start a short job, then consume the server stream — this exercises
	// WrapStreamingClient; an unauthenticated stream would fail immediately.
	res, err := cli.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{Command: "echo streaming-ok"}))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	stream, err := cli.WatchJob(ctx, connect.NewRequest(&workerv1.WatchJobRequest{JobId: res.Msg.JobId}))
	if err != nil {
		t.Fatalf("watch open: %v", err)
	}
	var got string
	var done bool
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *workerv1.WatchJobResponse_Output:
			got += ev.Output
		case *workerv1.WatchJobResponse_Done_:
			done = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("watch stream err (auth must be stripped for streams?): %v", err)
	}
	if !done {
		t.Fatal("stream did not terminate with Done")
	}
	_ = got
}
