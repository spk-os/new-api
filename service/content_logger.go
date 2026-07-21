package service

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// ContentLogEntry — a complete capture of one API request/response cycle
// ---------------------------------------------------------------------------

type HttpMessage struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Path    string            `json:"path,omitempty"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type ContentLogEntry struct {
	RequestID         string       `json:"request_id"`
	UserID            int          `json:"user_id"`
	ChannelID         int          `json:"channel_id"`
	ChannelName       string       `json:"channel_name"`
	ModelName         string       `json:"model_name"`
	UpstreamModelName string       `json:"upstream_model_name,omitempty"`
	CreatedAt         int64        `json:"created_at"`
	GatewayRequest    *HttpMessage `json:"gateway_request,omitempty"`
	GatewayResponse   *HttpMessage `json:"gateway_response,omitempty"`
	UpstreamRequest   *HttpMessage `json:"upstream_request,omitempty"`
	UpstreamResponse  *HttpMessage `json:"upstream_response,omitempty"`
	Gateway           *GatewayLog  `json:"gateway,omitempty"`
}

type GatewayLog struct {
	StrategyGroup   string   `json:"strategy_group"`
	ClientId        string   `json:"client_id"`
	TaskId          string   `json:"task_id"`
	StickyEnabled   bool     `json:"sticky_enabled"`
	AffinityStatus  string   `json:"affinity_status"`
	ProviderId      string   `json:"provider_id"`
	ModelGroup      string   `json:"model_group"`
	ActualModel     string   `json:"actual_model"`
	KeyIndex        int      `json:"key_index"`
	RetryCount      int      `json:"retry_count"`
	Cost            float64  `json:"cost"`
	CandidateChain  []string `json:"candidate_chain,omitempty"`
}

// ---------------------------------------------------------------------------
// ContentLogger — file-based writer with rotation
// ---------------------------------------------------------------------------

const (
	ContentLogDir        = "/data/content-logs"
	ContentLogFilePrefix = "content_"
	ContentLogFileSuffix = ".log"
	DefaultMaxSizeMB     = 100
	KeepUncompressedCount = 5
)

type ContentLogger struct {
	mu          sync.Mutex
	dir         string
	maxSize     int64
	currentFile *os.File
	writer      *bufio.Writer
	fileSize    int64
	fileSeq     int
	currentDate string
}

var globalContentLogger = &ContentLogger{
	dir:     ContentLogDir,
	maxSize: int64(DefaultMaxSizeMB) * 1024 * 1024,
}

func InitContentLogger() {
	if err := os.MkdirAll(globalContentLogger.dir, 0755); err != nil {
		common.SysLog("content_logger: failed to create dir: " + err.Error())
	}
}

func (cl *ContentLogger) effectiveMaxSize() int64 {
	mb := common.ContentLogMaxSizeMB
	if mb <= 0 {
		mb = DefaultMaxSizeMB
	}
	return int64(mb) * 1024 * 1024
}

// ---------------------------------------------------------------------------
// RecordContentLog saves a ContentLogEntry. Safe for concurrent use.
// ---------------------------------------------------------------------------

func RecordContentLog(entry *ContentLogEntry) {
	if entry == nil {
		return
	}
	if !common.ContentLogEnabled {
		return
	}

	cl := globalContentLogger

	cl.mu.Lock()
	defer cl.mu.Unlock()

	cl.maxSize = cl.effectiveMaxSize()

	if err := cl.ensureOpen(); err != nil {
		common.SysError("content_logger: ensureOpen: " + err.Error())
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		common.SysError("content_logger: marshal: " + err.Error())
		return
	}

	line := append(data, '\n')
	n := int64(len(line))

	if cl.fileSize+n > cl.maxSize && cl.fileSize > 0 {
		if err := cl.rotate(); err != nil {
			common.SysError("content_logger: rotate: " + err.Error())
			return
		}
		if err := cl.ensureOpen(); err != nil {
			common.SysError("content_logger: ensureOpen after rotate: " + err.Error())
			return
		}
	}

	if _, err := cl.writer.Write(line); err != nil {
		common.SysError("content_logger: write: " + err.Error())
		return
	}
	cl.fileSize += n
	_ = cl.writer.Flush()
}

// ---------------------------------------------------------------------------
// QueryContentLog retrieves a ContentLogEntry by request_id.
// ---------------------------------------------------------------------------

func QueryContentLog(requestID string) (*ContentLogEntry, error) {
	if requestID == "" {
		return nil, fmt.Errorf("empty request_id")
	}
	cl := globalContentLogger
	cl.mu.Lock()
	if cl.currentFile != nil {
		_ = cl.writer.Flush()
	}
	cl.mu.Unlock()

	files, err := filepath.Glob(filepath.Join(cl.dir, ContentLogFilePrefix+"*"+ContentLogFileSuffix))
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		fi, _ := os.Stat(files[i])
		fj, _ := os.Stat(files[j])
		if fi != nil && fj != nil {
			return fi.ModTime().After(fj.ModTime())
		}
		return files[i] > files[j]
	})

	prefix := []byte(`"request_id":"` + requestID + `"`)
	for _, fpath := range files {
		f, err := os.Open(fpath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if !containsBytes(line, prefix) {
				continue
			}
			var entry ContentLogEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}
			if entry.RequestID == requestID {
				_ = f.Close()
				return &entry, nil
			}
		}
		_ = f.Close()
	}

	zipFiles, err := filepath.Glob(filepath.Join(cl.dir, ContentLogFilePrefix+"*"+ContentLogFileSuffix+".zip"))
	if err != nil {
		return nil, nil
	}
	sort.Slice(zipFiles, func(i, j int) bool {
		fi, _ := os.Stat(zipFiles[i])
		fj, _ := os.Stat(zipFiles[j])
		if fi != nil && fj != nil {
			return fi.ModTime().After(fj.ModTime())
		}
		return zipFiles[i] > zipFiles[j]
	})

	for _, zpath := range zipFiles {
		zr, err := zip.OpenReader(zpath)
		if err != nil {
			continue
		}
		var found *ContentLogEntry
		for _, zf := range zr.File {
			rc, err := zf.Open()
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(rc)
			scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
			for scanner.Scan() {
				line := scanner.Bytes()
				if len(line) == 0 {
					continue
				}
				if !containsBytes(line, prefix) {
					continue
				}
				var entry ContentLogEntry
				if err := json.Unmarshal(line, &entry); err != nil {
					continue
				}
				if entry.RequestID == requestID {
					found = &entry
					break
				}
			}
			_ = rc.Close()
			if found != nil {
				break
			}
		}
		_ = zr.Close()
		if found != nil {
			return found, nil
		}
	}
	return nil, nil
}

func containsBytes(b, sub []byte) bool {
	return len(b) >= len(sub) && indexBytes(b, sub) >= 0
}

func indexBytes(b, sub []byte) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(b) {
		return -1
	}
	for i := 0; i <= len(b)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if b[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// file management helpers
// ---------------------------------------------------------------------------

func todayDate() string {
	return time.Now().Format("20060102")
}

func (cl *ContentLogger) ensureOpen() error {
	date := todayDate()
	if cl.currentFile != nil && cl.currentDate == date {
		return nil
	}
	cl.closeCurrent()

	fpath := filepath.Join(cl.dir, ContentLogFilePrefix+date+ContentLogFileSuffix)
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	cl.currentFile = f
	cl.writer = bufio.NewWriter(f)
	cl.fileSize = info.Size()
	cl.currentDate = date
	cl.fileSeq = 0
	return nil
}

func (cl *ContentLogger) rotate() error {
	cl.closeCurrent()
	date := todayDate()
	cl.fileSeq++

	oldPath := filepath.Join(cl.dir, ContentLogFilePrefix+date+ContentLogFileSuffix)
	newPath := filepath.Join(cl.dir, fmt.Sprintf("%s%s_%d%s", ContentLogFilePrefix, date, cl.fileSeq, ContentLogFileSuffix))
	if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	cl.cleanupOldFiles()
	return nil
}

func (cl *ContentLogger) closeCurrent() {
	if cl.writer != nil {
		_ = cl.writer.Flush()
		cl.writer = nil
	}
	if cl.currentFile != nil {
		_ = cl.currentFile.Close()
		cl.currentFile = nil
	}
	cl.fileSize = 0
	cl.currentDate = ""
}

func (cl *ContentLogger) cleanupOldFiles() {
	pattern := filepath.Join(cl.dir, ContentLogFilePrefix+"*"+ContentLogFileSuffix)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	sort.Strings(files)

	if len(files) <= KeepUncompressedCount {
		return
	}

	toCompress := files[:len(files)-KeepUncompressedCount]
	for _, f := range toCompress {
		zipPath := f + ".zip"
		if _, err := os.Stat(zipPath); err == nil {
			_ = os.Remove(f)
			continue
		}
		if err := compressFile(f, zipPath); err != nil {
			common.SysError("content_logger: compress failed for " + f + ": " + err.Error())
			continue
		}
		_ = os.Remove(f)
		common.SysLog("content_logger: compressed " + filepath.Base(f) + " -> " + filepath.Base(zipPath))
	}
}

func compressFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return err
	}

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	zw := zip.NewWriter(dst)
	defer zw.Close()

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Method = zip.Deflate
	header.Name = filepath.Base(srcPath)

	writer, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, src)
	return err
}

// ---------------------------------------------------------------------------
// ResponseWriter wrapper to capture gateway response body
// ---------------------------------------------------------------------------

type ContentLogResponseWriter struct {
	gin.ResponseWriter
	body       strings.Builder
	statusCode int
}

func NewContentLogResponseWriter(w gin.ResponseWriter) *ContentLogResponseWriter {
	return &ContentLogResponseWriter{
		ResponseWriter: w,
		statusCode:     200,
	}
}

func (w *ContentLogResponseWriter) StatusCode() int {
	return w.statusCode
}

func (w *ContentLogResponseWriter) BodyString() string {
	return w.body.String()
}

func (w *ContentLogResponseWriter) WriteHeader(status int) {
	w.statusCode = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *ContentLogResponseWriter) Write(data []byte) (int, error) {
	w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

func (w *ContentLogResponseWriter) WriteString(s string) (int, error) {
	w.body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

func (w *ContentLogResponseWriter) ResponseHeaders() map[string]string {
	h := make(map[string]string)
	for k, vv := range w.Header() {
		if len(vv) > 0 {
			h[k] = strings.Join(vv, "; ")
		}
	}
	return h
}

// ---------------------------------------------------------------------------
// Capture helpers — called from middleware and relay code
// ---------------------------------------------------------------------------

func CaptureGatewayRequest(c *gin.Context) *HttpMessage {
	msg := &HttpMessage{
		Method:  c.Request.Method,
		Path:    c.Request.URL.Path,
		Headers: sanitizeHeaders(flattenHeaders(c.Request.Header)),
	}
	body, err := common.GetBodyString(c)
	if err == nil && body != "" {
		msg.Body = body
	}
	return msg
}

func CaptureUpstreamRequest(method, url string, headers map[string]string, body string) *HttpMessage {
	return &HttpMessage{
		Method:  method,
		URL:     url,
		Headers: sanitizeHeaders(headers),
		Body:    body,
	}
}

func CaptureUpstreamResponse(status int, headers map[string]string, body string) *HttpMessage {
	return &HttpMessage{
		Status:  status,
		Headers: headers,
		Body:    body,
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func flattenHeaders(h map[string][]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, vv := range h {
		if len(vv) > 0 {
			out[k] = strings.Join(vv, "; ")
		}
	}
	return out
}

func sanitizeHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		kl := strings.ToLower(k)
		switch kl {
		case "authorization", "x-api-key", "x-goog-api-key", "cookie", "set-cookie", "api-key":
			out[k] = "***"
		default:
			out[k] = v
		}
	}
	return out
}
