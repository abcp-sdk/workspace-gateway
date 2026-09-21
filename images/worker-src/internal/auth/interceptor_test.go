package auth

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
)

func TestInterceptorBlocksThenAllows(t *testing.T) {
	g, _ := New(Options{})
	ic := NewInterceptor(g)
	next := func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	}
	h := ic.WrapUnary(next)

	req := connect.NewRequest(&struct{}{})
	if _, err := h(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unclaimed: err = %v, want Unauth", err)
	}

	tok, _ := g.Claim(g.Code(), "o")
	req2 := connect.NewRequest(&struct{}{})
	req2.Header().Set("Authorization", "Bearer "+tok)
	if _, err := h(context.Background(), req2); err != nil {
		t.Fatalf("authorized call failed: %v", err)
	}
}

var _ = http.Header{}
