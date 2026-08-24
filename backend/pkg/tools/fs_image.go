package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// imageMediaTypes maps the file extensions `read_image` accepts to their MIME
// types (mirrors upstream tool-fs/read-image.ts:22 IMAGE_EXTENSIONS). Magic-byte
// validation is not performed here; the declared extension is the gate, and the
// MIME type rides alongside the base64 payload for the vision model.
var imageMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

// RegisterImageTools registers the `read_image` tool. It is intentionally NOT
// wired into NewToolRegistry / pipeline.go — Phase 2 owns that wiring. Call it
// from a registry when the host route can actually consume vision results.
func (r *ToolRegistry) RegisterImageTools() {
	r.Register(ToolDefinition{
		Name:        "read_image",
		Description: "Read a PNG/JPEG/WebP/GIF file and return the image itself as base64 data with its MIME type. Requires the current model to accept image input.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"file_path": { "type": "string", "description": "Path to the image file, resolved against the session workspace" }
			},
			"required": ["file_path"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.FilePath) == "" {
				return nil, fmt.Errorf("file_path must be a non-empty string")
			}

			// Gate by declared extension before any filesystem I/O, mirroring
			// upstream read-image.ts:197 (a refusal never leaks partial reads).
			mediaType, ok := imageMediaTypes[strings.ToLower(filepath.Ext(args.FilePath))]
			if !ok {
				return nil, fmt.Errorf("cannot read %q: read_image only accepts PNG/JPEG/WebP/GIF paths", args.FilePath)
			}

			targetPath := resolvePath(ctx.Cwd, args.FilePath)

			// Read-only workspaces forbid image reads as well as writes.
			if r.Policy != nil && ctx.Cwd != "" {
				if marker := r.Policy.checkWrite(ctx.SessionID, ctx.Cwd, targetPath); marker != "" {
					return marker, nil
				}
			}

			data, err := os.ReadFile(targetPath)
			if err != nil {
				return nil, err
			}

			return map[string]any{
				"path":  targetPath,
				"image": base64.StdEncoding.EncodeToString(data),
				"mime":  mediaType,
			}, nil
		},
	})
}
