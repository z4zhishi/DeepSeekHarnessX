package mcp

import (
	"context"
	"encoding/json"
)

// RPC is one decoded JSON-RPC 2.0 message frame on the wire. It is the public
// shape shared by the MCP client (tools/list + tools/call) and the plugin host
// (capability/list + tool/register + tool/call + command/register +
// command/execute + event/subscribe). id == nil denotes a server notification.
type RPC struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// Transport is the generic JSON-RPC 2.0 transport shared by the MCP client and
// the plugin host. It abstracts one connected, fully-handshaken peer; reconnect
// orchestration (exponential backoff, connection-loss detection) lives above it
// in the Supervisor (MCP) and the plugin Host.
//
// A Transport is the transport boundary only: it knows nothing about the
// protocol method set layered on top. Callers encode their protocol (tools/list
// for MCP, capability/list for plugins) as method names passed to Call/Notify.
type Transport interface {
	// Call performs one request/response round trip and decodes the result into
	// out (nil to discard). Concurrent calls are permitted.
	Call(ctx context.Context, method string, params any, out any) error
	// Notify sends a one-way notification (no response expected).
	Notify(method string, params any)
	// Notifications delivers server-initiated messages (id == nil).
	Notifications() <-chan *RPC
	// Done fires when the peer disconnects / the process exits / the stream
	// terminates (a local Close does not fire it).
	Done() <-chan struct{}
	// Close tears down the transport and releases the peer process.
	Close() error
}
