package auth

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

// interceptor enforces bearer auth on the WorkerService handlers, while
// leaving the WorkerEnroll handlers untouched. It is a HandlerOption applied
// to NewWorkerServiceHandler.
//
// Fail-closed: every unary and streaming WorkerService call is rejected with
// Unauthenticated unless the request carries an accepted token. The enroll
// service is mounted separately (see cmd/easyworker) and manages its own gate.
type interceptor struct {
	gate *Gate
}

func NewInterceptor(g *Gate) connect.Interceptor { return &interceptor{gate: g} }

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.check(req.Header()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // server-side only
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.check(conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i *interceptor) check(h http.Header) error {
	if i.gate.Disabled() {
		return nil
	}
	tok := bearer(h.Get("Authorization"))
	if i.gate.Authorized(tok) {
		return nil
	}
	return connect.NewError(connect.CodeUnauthenticated, errUnauthenticated())
}

func bearer(header string) string {
	const p = "Bearer "
	if strings.HasPrefix(header, p) {
		return strings.TrimSpace(strings.TrimPrefix(header, p))
	}
	return ""
}

func errUnauthenticated() error {
	return errString("worker: unauthenticated (missing or invalid bearer token)")
}

type errString string

func (e errString) Error() string { return string(e) }
