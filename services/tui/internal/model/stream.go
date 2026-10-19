package model

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/caelum0x/ration/services/tui/internal/client"
)

// streamReconnectDelay is how long the background activity-stream reader
// waits before retrying after a failed Dial/Recv, so a gateway that isn't
// up yet (or restarts) doesn't spin the loop.
const streamReconnectDelay = 2 * time.Second

// activityMsg is one event off the gateway's StreamActivity RPC, still
// carrying the raw tenant_id — the tenant name lookup happens in Update,
// which is the only place m.tenantNames is read/written, so no locking is
// needed around it.
type activityMsg struct {
	id       string
	tenantID string
	kind     string
	summary  string
	atUnix   int64
}

// streamStatusMsg reports a connect/disconnect transition on the activity
// stream, rendered as the "● live" / "○ reconnecting…" footer indicator.
type streamStatusMsg struct {
	connected bool
	err       error
}

// startActivityStream launches the background stream-reading goroutine
// exactly once and returns immediately (nil message) — the goroutine talks
// to the rest of the program only by pushing onto events, never by calling
// back into the model directly, so there is no shared mutable state between
// it and the Bubble Tea update loop.
func startActivityStream(ctx context.Context, c *client.Clients, events chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		go activityStreamLoop(ctx, c, events)
		return nil
	}
}

func activityStreamLoop(ctx context.Context, c *client.Clients, events chan tea.Msg) {
	for {
		if ctx.Err() != nil {
			return
		}

		stream, err := c.StreamActivity(ctx)
		if err != nil {
			sendStatus(ctx, events, streamStatusMsg{connected: false, err: err})
			if !sleepOrDone(ctx, streamReconnectDelay) {
				return
			}
			continue
		}

		sendStatus(ctx, events, streamStatusMsg{connected: true})

		for {
			ev, err := stream.Recv()
			if err != nil {
				sendStatus(ctx, events, streamStatusMsg{connected: false, err: err})
				break
			}
			msg := tea.Msg(activityMsg{
				id:       ev.GetId(),
				tenantID: ev.GetTenantId(),
				kind:     ev.GetKind(),
				summary:  ev.GetSummary(),
				atUnix:   ev.GetAtUnix(),
			})
			select {
			case events <- msg:
			case <-ctx.Done():
				return
			default:
				// Feed is bounded; drop rather than block the gRPC reader
				// and stall the stream under burst load.
			}
		}

		if !sleepOrDone(ctx, streamReconnectDelay) {
			return
		}
	}
}

func sendStatus(ctx context.Context, events chan tea.Msg, msg streamStatusMsg) {
	select {
	case events <- tea.Msg(msg):
	case <-ctx.Done():
	}
}

// sleepOrDone waits for d, returning false early (without waiting) if ctx
// is canceled first.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitForActivity blocks for the next event pushed by activityStreamLoop
// and returns it as a tea.Msg. Update() must re-issue this command every
// time it handles the result, or the feed stops updating after one event.
func waitForActivity(events chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-events
		if !ok {
			return nil
		}
		return msg
	}
}
