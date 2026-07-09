package claws

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
)

// File upload limits for POST /api/files/{clawId}.
const (
	maxFileBytes    = 20 << 20 // 20MB per file
	maxTotalBytes   = 50 << 20 // 50MB per request
	maxFilesPerReq  = 10
	fileAckTimeout  = 30 * time.Second
	uploadFormField = "files"
)

// filePayload is what the hub sends to the claw-bridge over WS as type="file".
type filePayload struct {
	RequestID string `json:"request_id"`
	Filename  string `json:"filename"`
	MimeType  string `json:"mimetype"`
	Size      int64  `json:"size"`
	Data      string `json:"data"` // base64
}

type uploadedAttachment struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mimetype"`
}

// handleFileUpload accepts multipart/form-data under the "files" field and
// forwards each file to the claw-bridge over the existing WebSocket. It waits
// for matching file_ack frames and returns the on-disk paths for use in the
// subsequent message submission.
func (s *Service) HandleFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clawID := r.PathValue("clawID")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/files/")
	}
	if clawID == "" {
		http.Error(w, "missing claw id", http.StatusBadRequest)
		return
	}

	// Bound the request body before parsing multipart.
	r.Body = http.MaxBytesReader(w, r.Body, maxTotalBytes+(1<<20))
	if err := r.ParseMultipartForm(maxTotalBytes); err != nil {
		http.Error(w, "upload too large or malformed", http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File[uploadFormField]
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	if len(files) > maxFilesPerReq {
		http.Error(w, "too many files", http.StatusBadRequest)
		return
	}

	cc := s.reg.Lookup(clawID)
	if cc == nil {
		http.Error(w, "claw not connected", http.StatusConflict)
		return
	}

	tenantID := s.tenantFromCtx(r)
	if cc.TenantID != tenantID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	out := make([]uploadedAttachment, 0, len(files))
	for _, fh := range files {
		if fh.Size > maxFileBytes {
			http.Error(w, "file too large: "+fh.Filename, http.StatusRequestEntityTooLarge)
			return
		}
		f, err := fh.Open()
		if err != nil {
			http.Error(w, "open: "+fh.Filename, http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			http.Error(w, "read: "+fh.Filename, http.StatusBadRequest)
			return
		}
		mime := fh.Header.Get("Content-Type")
		if mime == "" {
			mime = "application/octet-stream"
		}

		reqID := uuid.New().String()
		ch := make(chan types.FileAck, 1)
		s.fileAckMu.Lock()
		s.fileAckWaiters()[reqID] = ch
		s.fileAckMu.Unlock()

		payload := filePayload{
			RequestID: reqID,
			Filename:  fh.Filename,
			MimeType:  mime,
			Size:      fh.Size,
			Data:      base64.StdEncoding.EncodeToString(data),
		}
		ctx, cancel := context.WithTimeout(r.Context(), fileAckTimeout)
		if err := wsjson.Write(ctx, cc.WS, types.WSMessage{Type: "file", Payload: payload}); err != nil {
			cancel()
			s.fileAckMu.Lock()
			delete(s.fileAckWaiters(), reqID)
			s.fileAckMu.Unlock()
			http.Error(w, "send to claw failed", http.StatusBadGateway)
			return
		}

		select {
		case ack := <-ch:
			cancel()
			if !ack.OK {
				http.Error(w, "claw rejected file: "+ack.Error, http.StatusBadGateway)
				return
			}
			out = append(out, uploadedAttachment{
				Name:     fh.Filename,
				Path:     ack.Path,
				Size:     fh.Size,
				MimeType: mime,
			})
		case <-ctx.Done():
			cancel()
			s.fileAckMu.Lock()
			delete(s.fileAckWaiters(), reqID)
			s.fileAckMu.Unlock()
			http.Error(w, "timeout waiting for claw ack", http.StatusGatewayTimeout)
			return
		}
	}

	s.jsonOK(w, map[string]interface{}{"files": out})
}

// isActiveContentType returns true if the content type or extension could
// execute scripts when rendered by a browser (e.g., SVG, HTML, XML).
func isActiveContentType(ct, ext string) bool {
	switch ext {
	case ".svg", ".html", ".htm", ".xhtml", ".xml", ".xsl", ".xslt":
		return true
	}
	if strings.HasPrefix(ct, "image/svg") ||
		strings.HasPrefix(ct, "text/html") ||
		strings.HasPrefix(ct, "application/xhtml") ||
		strings.HasPrefix(ct, "text/xml") ||
		strings.HasPrefix(ct, "application/xml") {
		return true
	}
	return false
}

// handleFileView streams the bytes of a previously-uploaded file back to the
// browser so images can render inline in chat history. The bridge enforces
// path containment within its uploads dir, so arbitrary filesystem reads are
// rejected at the claw even if a caller crafts a malicious path query.
func (s *Service) HandleFileView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clawID := r.PathValue("clawID")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/files/view/")
	}
	path := r.URL.Query().Get("path")
	if clawID == "" || path == "" {
		http.Error(w, "missing claw id or path", http.StatusBadRequest)
		return
	}

	cc := s.reg.Lookup(clawID)
	if cc == nil {
		http.Error(w, "claw not connected", http.StatusConflict)
		return
	}

	tenantID := s.tenantFromCtx(r)
	if cc.TenantID != tenantID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	reqID := uuid.New().String()
	ch := make(chan types.FileReadResp, 1)
	s.fileAckMu.Lock()
	s.fileReadWaiters()[reqID] = ch
	s.fileAckMu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), fileAckTimeout)
	defer cancel()

	if err := wsjson.Write(ctx, cc.WS, types.WSMessage{
		Type:    "file_read",
		Payload: map[string]string{"request_id": reqID, "path": path},
	}); err != nil {
		s.fileAckMu.Lock()
		delete(s.fileReadWaiters(), reqID)
		s.fileAckMu.Unlock()
		http.Error(w, "send to claw failed", http.StatusBadGateway)
		return
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			http.Error(w, "claw read failed: "+resp.Error, http.StatusBadGateway)
			return
		}
		data, err := base64.StdEncoding.DecodeString(resp.Data)
		if err != nil {
			http.Error(w, "decode failed", http.StatusBadGateway)
			return
		}
		ext := strings.ToLower(filepath.Ext(path))
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			ct = http.DetectContentType(data)
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")

		if isActiveContentType(ct, ext) {
			w.Header().Set("Content-Disposition", "attachment")
		}

		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		_, _ = w.Write(data)
	case <-ctx.Done():
		s.fileAckMu.Lock()
		delete(s.fileReadWaiters(), reqID)
		s.fileAckMu.Unlock()
		http.Error(w, "timeout waiting for claw", http.StatusGatewayTimeout)
	}
}
