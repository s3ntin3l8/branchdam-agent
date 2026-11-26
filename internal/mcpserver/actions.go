package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type startIngestIn struct {
	CardPath string `json:"cardPath,omitempty" jsonschema:"absolute folder to import; omit for Ingest now"`
}

type retryFailedIn struct {
	NodeUUID string `json:"nodeUuid,omitempty" jsonschema:"retry this failed row; set exactly one of nodeUuid or all"`
	All      bool   `json:"all,omitempty" jsonschema:"retry every failed row; set exactly one of nodeUuid or all"`
}

type setPausedIn struct {
	Paused bool `json:"paused" jsonschema:"true pauses ingest and drain for this tray session; false resumes"`
}

func registerActionTools(s *sdkmcp.Server, opts Options) {
	client := opts.tray()

	sdkmcp.AddTool(s, readOnlyTool("tray_status", "Read the running tray status JSON."+trayAvailabilityNote),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, map[string]any, error) {
			return client.call(ctx, http.MethodGet, "/api/status", nil)
		})

	sdkmcp.AddTool(s, actionTool("start_ingest", "Start an ingest through the tray. Omit cardPath to ingest every detected card volume. A paused, busy, or gate-held tray returns 200 JSON, not a tool error."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in startIngestIn) (*sdkmcp.CallToolResult, map[string]any, error) {
			return client.call(ctx, http.MethodPost, "/api/actions/ingest-start", map[string]string{
				"cardPath": in.CardPath,
			})
		})

	sdkmcp.AddTool(s, actionTool("cancel_ingest", "Ask the tray to stop the in-flight ingest at the next file boundary."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, map[string]any, error) {
			return client.call(ctx, http.MethodPost, "/api/actions/cancel-ingest", nil)
		})

	sdkmcp.AddTool(s, actionTool("drain_now", "Run one drain pass through the tray."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, map[string]any, error) {
			return client.call(ctx, http.MethodPost, "/api/actions/drain", nil)
		})

	sdkmcp.AddTool(s, actionTool("retry_failed", "Re-queue failed rows through the tray. Pass exactly one of nodeUuid or all."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in retryFailedIn) (*sdkmcp.CallToolResult, map[string]any, error) {
			body, err := retryBody(in)
			if err != nil {
				return nil, nil, err
			}
			return client.call(ctx, http.MethodPost, "/api/actions/queue-retry", body)
		})

	sdkmcp.AddTool(s, actionTool("set_paused", "Set the session-only pause gate through the tray. This does not change the persisted metered-upload setting."),
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in setPausedIn) (*sdkmcp.CallToolResult, map[string]any, error) {
			return client.call(ctx, http.MethodPost, "/api/actions/pause", map[string]bool{
				"paused": in.Paused,
			})
		})
}

func actionTool(name, description string) *sdkmcp.Tool {
	return &sdkmcp.Tool{
		Name:        name,
		Description: description + trayAvailabilityNote + untrustedNote,
		Annotations: &sdkmcp.ToolAnnotations{Title: name},
	}
}

func retryBody(in retryFailedIn) (any, error) {
	node := strings.TrimSpace(in.NodeUUID)
	switch {
	case in.All && node != "":
		return nil, errors.New("retry_failed requires exactly one of nodeUuid or all")
	case !in.All && node == "":
		return nil, errors.New("retry_failed requires exactly one of nodeUuid or all")
	case in.All:
		return map[string]bool{"all": true}, nil
	default:
		return map[string]string{"nodeUuid": node}, nil
	}
}
