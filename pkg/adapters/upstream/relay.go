package upstream

import (
	"context"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// relay forwards what the child server sends back up to the client that caused
// it. Progress notifications are correlated by the progress token the broker
// attaches to every forwarded call (the call id). Sampling and elicitation
// requests carry no correlation, so they are forwarded only while exactly one
// call is in flight on this child; otherwise they are refused. In stateless
// front mode the SDK rejects server→client requests outright, so sampling and
// elicitation need `--stateless=false` (progress still flows: it is a notification
// inside the request's own response stream).
type relay struct {
	mu       sync.Mutex
	inflight map[string]*inflightCall
}

type inflightCall struct {
	session       *mcp.ServerSession
	progressToken any // the client's own token, if it sent one
}

func newRelay() *relay { return &relay{inflight: map[string]*inflightCall{}} }

func (r *relay) begin(callID string, session *mcp.ServerSession, token any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight[callID] = &inflightCall{session: session, progressToken: token}
}

func (r *relay) end(callID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inflight, callID)
}

func (r *relay) byToken(token any) *inflightCall {
	id, ok := token.(string)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[id]
}

// sole returns the only in-flight call, or nil when there are zero or several.
func (r *relay) sole() *inflightCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inflight) != 1 {
		return nil
	}
	for _, c := range r.inflight {
		return c
	}
	return nil
}

func (r *relay) clientOptions() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			call := r.byToken(req.Params.ProgressToken)
			if call == nil || call.session == nil || call.progressToken == nil {
				return
			}
			_ = call.session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: call.progressToken, Message: req.Params.Message, Progress: req.Params.Progress, Total: req.Params.Total,
			})
		},
		CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			call := r.sole()
			if call == nil || call.session == nil {
				return nil, fmt.Errorf("brokerd: sampling request cannot be attributed to a single caller")
			}
			return call.session.CreateMessage(ctx, req.Params)
		},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			call := r.sole()
			if call == nil || call.session == nil {
				return nil, fmt.Errorf("brokerd: elicitation request cannot be attributed to a single caller")
			}
			return call.session.Elicit(ctx, req.Params)
		},
	}
}
