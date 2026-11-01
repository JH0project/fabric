package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"strconv"

	"github.com/JH0project/fabric/control/internal/environment"
)

const MaxRenderedBatchBytes = 8 * 1024 * 1024

type RenderedFile struct {
	Path     string `json:"path"`
	Rendered bool   `json:"rendered"`
	SHA256   string `json:"sha256,omitempty"`
	Code     string `json:"code,omitempty"`
}
type RenderResult struct {
	Rendered bool           `json:"rendered"`
	Files    []RenderedFile `json:"files"`
}

func (f File) ValidInput() bool {
	if f.Path == "" || (f.Content == nil) == (f.Template == nil) {
		return false
	}
	if f.Content != nil && len(*f.Content) > MaxFileBytes {
		return false
	}
	if f.Template != nil && len(f.Template.Source) > MaxTemplateBytes {
		return false
	}
	if f.ExpectedSHA256 != "" {
		b, err := hex.DecodeString(f.ExpectedSHA256)
		if err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != f.ExpectedSHA256 {
			return false
		}
	}
	return true
}
func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrTemplateInvalid):
		return "invalid_template"
	case errors.Is(err, ErrTemplateLimit):
		return "template_limit"
	case errors.Is(err, ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, environment.ErrUnavailable):
		return "environment_unavailable"
	case errors.Is(err, ErrInvalid):
		return "invalid_file"
	case errors.Is(err, ErrConflict):
		return "path_conflict"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	default:
		return "write_failed"
	}
}
func prepare(ctx context.Context, files []File) ([]File, RenderResult) {
	result := RenderResult{Rendered: true, Files: make([]RenderedFile, 0, len(files))}
	ready := make([]File, len(files))
	total := 0
	for i, file := range files {
		var content string
		err := ctx.Err()
		if err == nil {
			if !file.ValidInput() {
				err = ErrInvalid
			} else if file.Template != nil {
				content, err = RenderTemplate(ctx, *file.Template)
			} else {
				content = *file.Content
			}
		}
		digest := ""
		if err == nil {
			total += len(content)
			if total > MaxRenderedBatchBytes {
				err = ErrTemplateLimit
			} else {
				sum := sha256.Sum256([]byte(content))
				digest = hex.EncodeToString(sum[:])
				if file.ExpectedSHA256 != "" && file.ExpectedSHA256 != digest {
					err = ErrDigestMismatch
				}
			}
		}
		receipt := RenderedFile{Path: file.Path, Rendered: err == nil}
		if err != nil {
			result.Rendered = false
			receipt.Code = errorCode(err)
		} else {
			receipt.SHA256 = digest
			file.Content = &content
			file.preserveOwnership = file.Template != nil
			file.Template = nil
			file.ExpectedSHA256 = ""
			ready[i] = file
		}
		result.Files = append(result.Files, receipt)
	}
	return ready, result
}

// Render validates and renders the complete batch without accessing destination
// files. Its receipts allow a caller to preflight all nodes before applying.
func (s *Service) Render(ctx context.Context, request Request) RenderResult {
	_, result := s.prepare(ctx, request)
	return result
}

// Resolve external inputs once per request, then run the same pure preparation
// used by inline templates. No file can be written after a lookup failure.
func (s *Service) prepare(ctx context.Context, request Request) ([]File, RenderResult) {
	files := append([]File(nil), request.Files...)
	resolved := map[environment.Reference][]environment.Variable{}
	for i, file := range files {
		if file.Template == nil || file.Template.Environment == nil {
			continue
		}
		ref := *file.Template.Environment
		_, hasInline := file.Template.Data["env"]
		node, ok := file.Template.Data["node"].(map[string]any)
		if !ref.Valid() || ref.ApplicationUUID != request.ServiceID || hasInline || !ok || node["id"] != strconv.FormatInt(ref.ServerID, 10) || s.Environments == nil {
			return nil, environmentFailure(ctx, request.Files)
		}
		values, ok := resolved[ref]
		if !ok {
			var err error
			values, err = s.Environments.Resolve(ctx, ref)
			if err != nil {
				return nil, environmentFailure(ctx, request.Files)
			}
			resolved[ref] = values
		}
		data := make(map[string]any, len(file.Template.Data)+1)
		for k, v := range file.Template.Data {
			data[k] = v
		}
		env := make([]any, 0, len(values))
		for _, v := range values {
			env = append(env, map[string]any{"key": v.Key, "value": v.Value})
		}
		data["env"] = env
		spec := *file.Template
		spec.Data = data
		spec.Environment = nil
		files[i].Template = &spec
	}
	return prepare(ctx, files)
}

func environmentFailure(ctx context.Context, files []File) RenderResult {
	result := RenderResult{Files: make([]RenderedFile, 0, len(files))}
	err := ctx.Err()
	if err == nil {
		err = environment.ErrUnavailable
	}
	for _, file := range files {
		result.Files = append(result.Files, RenderedFile{Path: file.Path, Code: errorCode(err)})
	}
	return result
}
