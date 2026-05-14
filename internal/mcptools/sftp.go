package mcptools

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func RegisterSFTP(srv *server.MCPServer, st *State) {
	srv.AddTool(mcp.NewTool("ssh_file_list",
		mcp.WithDescription("List a directory on the remote host via SFTP."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
	), handleFileList(st))

	srv.AddTool(mcp.NewTool("ssh_file_stat",
		mcp.WithDescription("Stat a remote file."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
	), handleFileStat(st))

	srv.AddTool(mcp.NewTool("ssh_file_read",
		mcp.WithDescription("Read a remote text file. Returns up to 64 MiB of UTF-8 content. For binary blobs or anything large, use ssh_download to stream to disk instead — round-tripping bytes through this tool is wasteful and breaks on non-UTF-8 data."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithNumber("offset", mcp.Description("Byte offset to start reading from (default 0).")),
		mcp.WithNumber("length", mcp.Description("Max bytes to read (default: full file, capped at 64 MiB).")),
	), handleFileRead(st))

	srv.AddTool(mcp.NewTool("ssh_file_write",
		mcp.WithDescription("Write text content to a remote file. For binary content or anything larger than a few KiB, use ssh_upload to stream from a local file — embedding payloads in this argument inflates the JSON-RPC message (and trips MCP size limits)."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("data", mcp.Required(), mcp.Description("Text content to write.")),
		mcp.WithBoolean("append", mcp.Description("Append rather than truncate the existing file (default false).")),
	), handleFileWrite(st))

	srv.AddTool(mcp.NewTool("ssh_file_delete",
		mcp.WithDescription("Delete a remote file."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
	), handleFileDelete(st))

	srv.AddTool(mcp.NewTool("ssh_file_mkdir",
		mcp.WithDescription("Create a remote directory."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithBoolean("recursive", mcp.Description("Create parents as needed (mkdir -p).")),
	), handleFileMkdir(st))

	srv.AddTool(mcp.NewTool("ssh_file_chmod",
		mcp.WithDescription("Change remote file permissions. Pass `mode` as a string in octal: \"0755\", \"755\", or \"0o755\". (Numeric mode is also accepted but interpreted as decimal — 0o755 = 493 — which is almost always the wrong thing.)"),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("path", mcp.Required()),
		mcp.WithString("mode", mcp.Required(), mcp.Description("Octal permission bits: \"0755\", \"755\", or \"0o755\".")),
	), handleFileChmod(st))

	srv.AddTool(mcp.NewTool("ssh_file_rename",
		mcp.WithDescription("Rename or move a remote file."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("from", mcp.Required()),
		mcp.WithString("to", mcp.Required()),
	), handleFileRename(st))

	srv.AddTool(mcp.NewTool("ssh_upload",
		mcp.WithDescription("Stream a local file OR directory (where the daemon runs) to the remote host via SFTP. Directories are mirrored recursively; intermediate dirs are created; file permissions are preserved best-effort; symlinks are skipped. This is the right tool for any non-trivial data transfer — it streams over the SSH channel without serializing bytes into the MCP message."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("local_path", mcp.Required(), mcp.Description("Path on the daemon's filesystem; can be a file or a directory.")),
		mcp.WithString("remote_path", mcp.Required(), mcp.Description("Destination path on the remote host. When local_path is a directory, this becomes the remote root for the mirrored tree.")),
	), handleUpload(st))

	srv.AddTool(mcp.NewTool("ssh_download",
		mcp.WithDescription("Stream a remote file OR directory to the local filesystem (where the daemon runs) via SFTP. Directories are mirrored recursively; intermediate dirs are created; symlinks are skipped. Use this instead of ssh_file_read whenever the payload is binary or larger than a few KiB."),
		mcp.WithString("session_id", mcp.Required()),
		mcp.WithString("remote_path", mcp.Required(), mcp.Description("Path on the remote host; can be a file or a directory.")),
		mcp.WithString("local_path", mcp.Required(), mcp.Description("Destination path on the daemon's filesystem. When remote_path is a directory, this becomes the local root for the mirrored tree.")),
	), handleDownload(st))
}

func handleFileList(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		entries, err := s.FileList(path)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(entries)
	}
}

func handleFileStat(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		info, err := s.FileStat(path)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(info)
	}
}

func handleFileRead(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		offset := int64(req.GetInt("offset", 0))
		length := int64(req.GetInt("length", 0))
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		data, err := s.FileRead(path, offset, length)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes": len(data), "data": string(data)})
	}
}

func handleFileWrite(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		data, err := req.RequireString("data")
		if err != nil {
			return resultErr(err)
		}
		appendMode := req.GetBool("append", false)
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.FileWrite(path, []byte(data), appendMode); err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes_written": len(data)})
	}
}

func handleFileDelete(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.FileDelete(path); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("deleted " + path), nil
	}
}

func handleFileMkdir(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		recursive := req.GetBool("recursive", false)
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.FileMkdir(path, recursive); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("created " + path), nil
	}
}

func handleFileChmod(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		path, err := req.RequireString("path")
		if err != nil {
			return resultErr(err)
		}
		modeStr := req.GetString("mode", "")
		var mode os.FileMode
		if modeStr != "" {
			mode, err = parseOctalMode(modeStr)
			if err != nil {
				return resultErr(err)
			}
		} else {
			// Back-compat numeric form. We DO NOT interpret as octal — bare
			// numbers are decimal, but we reject anything that doesn't have
			// a value-looking-like-permission-bits to surface the footgun.
			n, ierr := req.RequireInt("mode")
			if ierr != nil {
				return resultErr(errors.New("`mode` is required (use a string like \"0755\")"))
			}
			if n < 0 || n > 0o7777 {
				return resultErr(errors.New("`mode` out of permission-bit range; pass as a string like \"0755\""))
			}
			mode = os.FileMode(n)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.FileChmod(path, mode); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("chmod ok"), nil
	}
}

func parseOctalMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty mode")
	}
	// Accept "0755", "755", "0o755".
	if len(s) >= 2 && (s[:2] == "0o" || s[:2] == "0O") {
		s = s[2:]
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, errors.New("mode must be octal: \"0755\", \"755\", or \"0o755\"")
	}
	if n > 0o7777 {
		return 0, errors.New("mode out of permission-bit range")
	}
	return os.FileMode(n), nil
}

func handleFileRename(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		from, err := req.RequireString("from")
		if err != nil {
			return resultErr(err)
		}
		to, err := req.RequireString("to")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		if err := s.FileRename(from, to); err != nil {
			return resultErr(err)
		}
		return mcp.NewToolResultText("renamed"), nil
	}
}

func handleUpload(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		local, err := req.RequireString("local_path")
		if err != nil {
			return resultErr(err)
		}
		remote, err := req.RequireString("remote_path")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		n, err := s.Upload(local, remote)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes": n})
	}
}

func handleDownload(st *State) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sid, err := req.RequireString("session_id")
		if err != nil {
			return resultErr(err)
		}
		remote, err := req.RequireString("remote_path")
		if err != nil {
			return resultErr(err)
		}
		local, err := req.RequireString("local_path")
		if err != nil {
			return resultErr(err)
		}
		s, err := st.SSH.Get(sid)
		if err != nil {
			return resultErr(err)
		}
		n, err := s.Download(remote, local)
		if err != nil {
			return resultErr(err)
		}
		return st.resultJSON(map[string]any{"bytes": n})
	}
}
