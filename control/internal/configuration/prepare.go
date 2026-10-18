package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
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
	_, result := prepare(ctx, request.Files)
	return result
}
