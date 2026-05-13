package daemon

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mark3labs/mcp-go/server"
)

// RPCHandler exposes the MCP JSON-RPC surface as a plain request/response HTTP
// endpoint, so a CLI client can POST a single JSON-RPC request and get a single
// JSON-RPC response back inline — no SSE bookkeeping. The launcher's stdio
// proxy still drives /sse + /message as before; /rpc is a parallel transport
// for one-shot use (and the same handler runs every tool, so behavior is
// identical to what an MCP client sees).
//
// Auth is unchanged: this handler sits behind AuthMiddleware.
//
// Notifications (JSON-RPC requests without an "id") return 204 No Content
// because they have no response body by protocol.
func RPCHandler(mcp *server.MCPServer) http.Handler {
	const maxBody = 4 << 20 // 4 MiB — comfortably larger than any plausible tool args
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		resp := mcp.HandleMessage(r.Context(), body)
		w.Header().Set("Content-Type", "application/json")
		if resp == nil {
			// Notification — no response body by spec.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			// Headers already flushed in the worst case; nothing useful to do
			// beyond logging. The client will see a truncated body.
			return
		}
	})
}
