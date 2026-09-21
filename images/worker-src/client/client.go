// Package client is the reusable easyworker client: it performs the one-time
// exclusive enrollment handshake and builds a bearer-authenticated
// WorkerService client. Any service (easylab, a controller, an operator CLI)
// can import it without depending on easylab.
//
// Enrollment (external sandbox / host runner path):
//
//	tok, err := client.Enroll(ctx, "http://host:8080", code, "my-service")
//	cli := client.Dial("http://host:8080", tok) // workerv1connect.WorkerServiceClient
//
// Managed sandboxes instead receive WORKER_TOKEN via env and only need:
//
//	cli := client.Dial("http://127.0.0.1:PORT", tok)
package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/easylab-platform/easyworker/gen/worker/v1"
	"github.com/easylab-platform/easyworker/gen/worker/v1/workerv1connect"
)

// Enroll performs the one-time exclusive claim against an unclaimed worker and
// returns the worker-issued bearer token. ownerID is recorded by the worker for
// audit (may be empty).
//
// A worker that is already claimed (or pre-authorized) returns
// ErrAlreadyClaimed — enrollment is exclusive and cannot be transferred.
func Enroll(ctx context.Context, baseURL, code, ownerID string) (string, error) {
	ec := NewEnrollClient(baseURL)
	res, err := ec.Claim(ctx, connect.NewRequest(&workerv1.EnrollClaimRequest{
		Code: code, OwnerId: ownerID,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			return "", ErrAlreadyClaimed
		}
		return "", err
	}
	if res.Msg == nil || res.Msg.Token == "" {
		return "", fmt.Errorf("worker returned no token")
	}
	return res.Msg.Token, nil
}

// Status queries the worker's enrollment state (useful to detect a restarted,
// unclaimed worker).
func Status(ctx context.Context, baseURL string) (*workerv1.EnrollStatusResponse, error) {
	res, err := NewEnrollClient(baseURL).Status(ctx, connect.NewRequest(&workerv1.EnrollStatusRequest{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Bearer returns a connect.ClientOption that attaches `Authorization: Bearer
// <token>` to every request — unary AND streaming. The streaming side is
// essential: connect.UnaryInterceptorFunc is a no-op for streams, so a
// unary-only interceptor would leave server-streaming RPCs (WatchJob)
// unauthenticated against the worker's fail-closed gate.
func Bearer(token string) connect.ClientOption {
	return connect.WithInterceptors(bearerInterceptor{token: token})
}

type bearerInterceptor struct{ token string }

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		return conn
	}
}

// Server-side methods are no-ops: this client only originates calls.
func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// Dial builds a bearer-authenticated WorkerService client.
func Dial(baseURL, token string, opts ...connect.ClientOption) workerv1connect.WorkerServiceClient {
	hc := &http.Client{}
	return workerv1connect.NewWorkerServiceClient(hc, trimSlash(baseURL), append(opts, Bearer(token))...)
}

// NewEnrollClient builds a WorkerEnroll client (no auth; the claim face is
// open only while the worker is unclaimed).
func NewEnrollClient(baseURL string) workerv1connect.WorkerEnrollClient {
	return workerv1connect.NewWorkerEnrollClient(&http.Client{}, trimSlash(baseURL))
}

// ErrAlreadyClaimed is returned when the worker already has an owner.
var ErrAlreadyClaimed = errors.New("worker already claimed by another caller")

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Release revokes the worker's current token (must equal bearer) and returns a
// fresh one-time code, putting the worker back into the claimable state. Only
// a claimed-by-code worker can be released (managed/pre-authorized cannot).
func Release(ctx context.Context, baseURL, bearer, ownerID string) (string, error) {
	hc := &http.Client{Timeout: 15 * time.Second}
	opts := []connect.ClientOption{}
	if bearer != "" {
		opts = append(opts, Bearer(bearer))
	}
	res, err := workerv1connect.NewWorkerEnrollClient(hc, trimSlash(baseURL), opts...).
		Unrelease(ctx, connect.NewRequest(&workerv1.EnrollUnreleaseRequest{OwnerId: ownerID}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetCode(), nil
}
