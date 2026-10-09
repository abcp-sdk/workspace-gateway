package workspacesvc

import (
	"context"
	"fmt"
	"log"

	"connectrpc.com/connect/v2"
	abcagent "github.com/abcp-sdk/abc-protocol-go/v2/agent"
	wsv1 "github.com/abcp-sdk/workspace-gateway/gen/workspace/v1"
	"github.com/abcp-sdk/workspace-gateway/internal/roles"
)

// notifyMRSubmitted wakes the TARGET branch's session with a mailbox `trigger`
// so the reviewer (e.g. the main-branch maintainer) learns about a new change
// request instead of polling. It mirrors the extension's `repo-mr-create`
// notification, but lives in the gateway so the `sandbox-submit-mr` path (which
// only calls SubmitMR) is covered too.
//
// Best-effort: every failure is logged and swallowed — a notification must
// never fail a submitted MR. Skipped when the target IS the submitting session
// (nothing to notify) or when no bus is configured.
func (s *Service) notifyMRSubmitted(ctx context.Context, hdr *connect.Header, tenant, org, repo, base string, index int32, title, url string) {
	if s.bus == nil {
		return
	}
	if base == "" {
		base = roles.MainBranch
	}
	baseSession := roles.SessionName(org, repo, base)
	submitter := sessionFromHeaders(hdr)
	if baseSession == submitter {
		return // the submitter IS the base session; nothing to notify
	}
	// Ensure the base session exists so its mailbox is durable. Copy the
	// caller's headers so tenant/identity resolution matches the submit.
	ereq := &wsv1.EnsureBranchSessionRequest{Org: org, Repo: repo, Branch: base}
	ctx, info := connect.NewClientContext(ctx)
	if hdr != nil {
		for k, vs := range hdr.All() {
			for _, v := range vs {
				info.RequestHeader().Add(k, v)
			}
		}
	}
	if _, err := s.EnsureBranchSession(ctx, ereq); err != nil {
		log.Printf("mr-notify: ensure base session %s: %v", baseSession, err)
	}
	text := fmt.Sprintf("New change request #%d in %s/%s: %q (→ %s).\n%s", index, org, repo, title, base, url)
	if err := abcagent.New(s.bus).PublishMailbox(ctx, tenant, baseSession, "trigger",
		map[string]any{"text": text}, "system:repo-mr"); err != nil {
		log.Printf("mr-notify: publish to %s: %v", baseSession, err)
	}
}
