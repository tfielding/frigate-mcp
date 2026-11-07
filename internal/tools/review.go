package tools

// review_alert: everything for judging whether an alert (or tracked object) was real, in one
// call: the timeline facts of get_event_timeline and the picture of get_event_frames.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (t *Toolset) reviewAlert(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	o, err := t.resolveObjects(ctx, "review_alert", id)
	if err != nil {
		return nil, nil, err
	}
	timeline, err := t.timelineText(ctx, "review_alert", id, o)
	if err != nil {
		return nil, nil, err
	}
	caption, jpg, err := t.framesPicture(ctx, "review_alert", id, o)
	if err != nil {
		// The facts alone still help; say why there's no picture.
		slog.Warn("review_alert: no picture", "id", id, "err", err)
		return text(strings.TrimSpace(timeline) + "\n\nNo picture: " + err.Error()), nil, nil
	}
	return imageResult(strings.TrimSpace(timeline)+"\n\n"+caption, jpg), nil, nil
}
