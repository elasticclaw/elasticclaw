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

	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
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
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	clawID := r.PathValue("clawID")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/files/")
	}
	if clawID == "" {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "missing claw id")
		return
	}

	// Bound the request body before parsing multipart.
	r.Body = http.MaxBytesReader(w, r.Body, maxTotalBytes+(1<<20))
	if err := r.ParseMultipartForm(maxTotalBytes); err != nil {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "upload too large or malformed")
		return
	}
	files := r.MultipartForm.File[uploadFormField]
	if len(files) == 0 {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "no files")
		return
	}
	if len(files) > maxFilesPerReq {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "too many files")
		return
	}

	cc := s.reg.Lookup(clawID)
	if cc == nil {
		httpserver.WriteErr(w, http.StatusConflict, "conflict", "claw not connected")
		return
	}

	tenantID := s.tenantFromCtx(r)
	if cc.TenantID != tenantID {
		httpserver.WriteErr(w, http.StatusForbidden, "forbidden", "forbidden")
		return
	}

	out := make([]uploadedAttachment, 0, len(files))
	for _, fh := range files {
		if fh.Size > maxFileBytes {
			httpserver.WriteErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "file too large: "+fh.Filename)
			return
		}
		f, err := fh.Open()
		if err != nil {
			httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "open: "+fh.Filename)
			return
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "read: "+fh.Filename)
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
			httpserver.WriteErr(w, http.StatusBadGateway, "bad_gateway", "send to claw failed")
			return
		}

		select {
		case ack := <-ch:
			cancel()
			if !ack.OK {
				httpserver.WriteErr(w, http.StatusBadGateway, "bad_gateway", "claw rejected file: "+ack.Error)
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
			httpserver.WriteErr(w, http.StatusGatewayTimeout, "gateway_timeout", "timeout waiting for claw ack")
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
		httpserver.WriteErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	clawID := r.PathValue("clawID")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/files/view/")
	}
	path := r.URL.Query().Get("path")
	if clawID == "" || path == "" {
		httpserver.WriteErr(w, http.StatusBadRequest, "bad_request", "missing claw id or path")
		return
	}

	cc := s.reg.Lookup(clawID)
	if cc == nil {
		httpserver.WriteErr(w, http.StatusConflict, "conflict", "claw not connected")
		return
	}

	tenantID := s.tenantFromCtx(r)
	if cc.TenantID != tenantID {
		httpserver.WriteErr(w, http.StatusForbidden, "forbidden", "forbidden")
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
		httpserver.WriteErr(w, http.StatusBadGateway, "bad_gateway", "send to claw failed")
		return
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			httpserver.WriteErr(w, http.StatusBadGateway, "bad_gateway", "claw read failed: "+resp.Error)
			return
		}
		data, err := base64.StdEncoding.DecodeString(resp.Data)
		if err != nil {
			httpserver.WriteErr(w, http.StatusBadGateway, "bad_gateway", "decode failed")
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
		httpserver.WriteErr(w, http.StatusGatewayTimeout, "gateway_timeout", "timeout waiting for claw")
	}
}
