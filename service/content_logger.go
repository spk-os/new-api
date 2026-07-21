package service

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
	Method  string             `json:"method,omitempty"`
	URL    string             `json:"url,omitempty"`
	Path   string             `json:"path,omitempty"`
	Status int                `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body   string             `json:"body,omitempty"`
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
	StrategyGroup  string   `json:"strategy_group"`
	ClientId       string   `json:"client_id"`
	TaskId         string   `json:"task_id"`
	StickyEnabled  bool     `json:"sticky_enabled"`
	AffinityStatus string   `json:"affinity_status"`
	ProviderId     string   `json:"provider_id"`
	ModelGroup     string   `json:"model_group"`
	ActualModel    string   `json:"actual_model"`
	KeyIndex       int      `json:"key_index"`
	RetryCount     int      `json:"retry_count"`
	Cost           float64  `json:"cost"`
	CandidateChain []string `json:"candidate_chain,omitempty"`
}

// ---------------------------------------------------------------------------
// ContentLogger — file-based writer with rotation, gzip compression,
// and timestamp-prefixed query for fast request_id lookup.
// ---------------------------------------------------------------------------

const (
	// ContentLogDir is the new storage directory for content logs.
	ContentLogDir = "/root/share/ops/logs/new-api"
	// OldContentLogDir is the pre-migration directory; files are moved to ContentLogDir at startup.
	OldContentLogDir = "/data/content-logs"
	// ContentLogFileSuffix is the extension for active (uncompressed) log files.
	ContentLogFileSuffix = ".log"
	// ContentLogGzipSuffix is the extension for gzip-compressed log files.
	ContentLogGzipSuffix = ".log.gz"
	// OldContentLogFilePrefix is the filename prefix used by the legacy naming convention.
	OldContentLogFilePrefix = "content_"
	// OldContentLogZipSuffix is the extension for legacy zip-compressed log files.
	OldContentLogZipSuffix = ".log.zip"
	// DefaultMaxSizeMB is the default maximum size of a single log file before rotation.
	DefaultMaxSizeMB = 100
	// KeepUncompressedCount is the maximum number of uncompressed .log files to retain.
	KeepUncompressedCount = 5
	// CompressAgeDays is the age threshold (in days) beyond which uncompressed files are gzip-compressed.
	CompressAgeDays = 7
)

// contentLogFileTimeFormat is the Go time format used for filenames (UTC).
// Produces filenames like: 20260721-143050.123.log
const contentLogFileTimeFormat = "20060102-150405.000"

// contentLogFileRe matches new-format filenames: YYYYMMDD-HHmmss.SSS.log
var contentLogFileRe = regexp.MustCompile(`^\d{8}-\d{6}\.\d{3}\.log$`)

type ContentLogger struct {
	mu          sync.Mutex
	dir         string
	maxSize     int64
	currentFile *os.File
	writer      *bufio.Writer
	fileSize    int64
	currentName string
}

var globalContentLogger = &ContentLogger{
	dir:     ContentLogDir,
	maxSize: int64(DefaultMaxSizeMB) * 1024 * 1024,
}

func InitContentLogger() {
	if envDir := os.Getenv("CONTENT_LOG_DIR"); envDir != "" {
		globalContentLogger.dir = envDir
	}
	if err := os.MkdirAll(globalContentLogger.dir, 0755); err != nil {
		common.SysLog("content_logger: failed to create dir: " + err.Error())
	}
	migrateOldLogs()
}

// migrateOldLogs moves files from the legacy directory to the new directory.
func migrateOldLogs() {
	oldDir := OldContentLogDir
	newDir := globalContentLogger.dir
	if oldDir == newDir {
		return
	}
	entries, err := os.ReadDir(oldDir)
	if err != nil {
		return
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		oldPath := filepath.Join(oldDir, entry.Name())
		newPath := filepath.Join(newDir, entry.Name())
		if _, err := os.Stat(newPath); err == nil {
			continue
		}
		if err := moveFile(oldPath, newPath); err != nil {
			common.SysLog("content_logger: failed to migrate " + entry.Name() + ": " + err.Error())
		} else {
			count++
		}
	}
	if count > 0 {
		common.SysLog(fmt.Sprintf("content_logger: migrated %d file(s) from %s to %s", count, oldDir, newDir))
	}
}

// moveFile tries os.Rename and falls back to copy+delete for cross-device moves.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := io.Copy(df, sf); err != nil {
		return err
	}
	return os.Remove(src)
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

	data, err := common.Marshal(entry)
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
//
// Lookup strategy (fastest to slowest):
//  1. Extract UTC timestamp from request_id prefix, narrow candidate files
//     to those opened around that timestamp, scan only those.
//  2. Fallback: scan ALL .log files (including legacy content_*.log).
//  3. Fallback: scan .log.gz files (gzip-compressed new-format).
//  4. Fallback: scan .log.zip files (legacy zip-compressed).
// ---------------------------------------------------------------------------

func QueryContentLog(requestID string) (*ContentLogEntry, error) {
	if requestID == "" {
		return nil, fmt.Errorf("empty request_id")
	}
	cl := globalContentLogger
	cl.mu.Lock()
	if cl.currentFile != nil && cl.writer != nil {
		_ = cl.writer.Flush()
	}
	cl.mu.Unlock()

	prefix := []byte(`"request_id":"` + requestID + `"`)

	// 1. Timestamp-based narrow lookup on new-format .log files
	if reqTs, ok := extractRequestIdTimestamp(requestID); ok {
		if entry := cl.queryByTimestamp(reqTs, prefix, requestID); entry != nil {
			return entry, nil
		}
	}

	// 2. Fallback: scan all .log files (including legacy content_*.log)
	if entry := cl.scanAllLogFiles(prefix, requestID); entry != nil {
		return entry, nil
	}

	// 3. Scan .log.gz files (new gzip-compressed)
	if entry := cl.scanGzipFiles(prefix, requestID); entry != nil {
		return entry, nil
	}

	// 4. Scan .log.zip files (legacy zip-compressed)
	if entry := cl.scanOldZipFiles(prefix, requestID); entry != nil {
		return entry, nil
	}

	return nil, nil
}

// extractRequestIdTimestamp parses the UTC timestamp embedded in a request_id.
// request_id format: YYYYMMDDHHmmSS + 9-digit nanos + hash + random (>=17 chars).
// Returns the parsed UTC time (millisecond precision).
func extractRequestIdTimestamp(requestID string) (time.Time, bool) {
	if len(requestID) < 17 {
		return time.Time{}, false
	}
	tsStr := requestID[:14] + "." + requestID[14:17]
	t, err := time.ParseInLocation("20060102150405.000", tsStr, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// queryByTimestamp narrows candidate files to those whose name-timestamp is
// near the request's timestamp, then scans only those files.
func (cl *ContentLogger) queryByTimestamp(reqTs time.Time, prefix []byte, requestID string) *ContentLogEntry {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return nil
	}

	var logFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if contentLogFileRe.MatchString(name) {
			logFiles = append(logFiles, name)
		}
	}
	sort.Strings(logFiles)

	if len(logFiles) == 0 {
		return nil
	}

	// The file open at the time of the request has a timestamp <= reqTs.
	// The content log may have been written to that file or a subsequent
	// one (if rotation happened during the request). Check 3 candidates.
	reqTsStr := reqTs.Format(contentLogFileTimeFormat) + ContentLogFileSuffix
	idx := sort.SearchStrings(logFiles, reqTsStr)
	start := idx - 1
	if start < 0 {
		start = 0
	}
	end := start + 3
	if end > len(logFiles) {
		end = len(logFiles)
	}

	for i := start; i < end; i++ {
		fpath := filepath.Join(cl.dir, logFiles[i])
		if entry := scanFileForRequestID(fpath, prefix, requestID); entry != nil {
			return entry
		}
	}
	return nil
}

// scanAllLogFiles scans every .log file in the directory (both new and legacy format).
func (cl *ContentLogger) scanAllLogFiles(prefix []byte, requestID string) *ContentLogEntry {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return nil
	}

	var logFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ContentLogFileSuffix) && !strings.HasSuffix(name, ContentLogGzipSuffix) {
			logFiles = append(logFiles, name)
		}
	}
	// Newest first — recent entries are more likely to be queried.
	sort.Sort(sort.Reverse(sort.StringSlice(logFiles)))

	for _, name := range logFiles {
		fpath := filepath.Join(cl.dir, name)
		if entry := scanFileForRequestID(fpath, prefix, requestID); entry != nil {
			return entry
		}
	}
	return nil
}

// scanGzipFiles scans all .log.gz files for the request_id.
func (cl *ContentLogger) scanGzipFiles(prefix []byte, requestID string) *ContentLogEntry {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return nil
	}

	var gzFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ContentLogGzipSuffix) {
			gzFiles = append(gzFiles, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(gzFiles)))

	for _, name := range gzFiles {
		fpath := filepath.Join(cl.dir, name)
		if entry := scanGzipFileForRequestID(fpath, prefix, requestID); entry != nil {
			return entry
		}
	}
	return nil
}

// scanOldZipFiles scans legacy .log.zip files for the request_id.
func (cl *ContentLogger) scanOldZipFiles(prefix []byte, requestID string) *ContentLogEntry {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, OldContentLogZipSuffix) {
			continue
		}
		fpath := filepath.Join(cl.dir, name)
		if entry := scanZipFileForRequestID(fpath, prefix, requestID); entry != nil {
			return entry
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// file scanning helpers
// ---------------------------------------------------------------------------

func scanFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !bytes.Contains(line, prefix) {
			continue
		}
		var entry ContentLogEntry
		if err := common.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.RequestID == requestID {
			return &entry
		}
	}
	return nil
}

func scanGzipFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer gz.Close()

	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if !bytes.Contains(line, prefix) {
			continue
		}
		var entry ContentLogEntry
		if err := common.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.RequestID == requestID {
			return &entry
		}
	}
	return nil
}

func scanZipFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil
	}
	defer zr.Close()

	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(rc)
		scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
		found := false
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if !bytes.Contains(line, prefix) {
				continue
			}
			var entry ContentLogEntry
			if err := common.Unmarshal(line, &entry); err != nil {
				continue
			}
			if entry.RequestID == requestID {
				_ = rc.Close()
				return &entry
			}
		}
		_ = rc.Close()
		if found {
			break
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// file management helpers
// ---------------------------------------------------------------------------

func (cl *ContentLogger) ensureOpen() error {
	if cl.currentFile != nil {
		return nil
	}

	now := time.Now().UTC()
	name := now.Format(contentLogFileTimeFormat) + ContentLogFileSuffix
	fpath := filepath.Join(cl.dir, name)
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
	cl.currentName = name
	return nil
}

func (cl *ContentLogger) rotate() error {
	cl.closeCurrent()
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
	cl.currentName = ""
}

// cleanupOldFiles gzip-compresses and removes old uncompressed .log files:
//   - Files older than CompressAgeDays are compressed.
//   - If more than KeepUncompressedCount files remain, the oldest excess files
//     are also compressed.
//
// Only new-format files (YYYYMMDD-HHmmss.SSS.log) are managed; legacy
// content_*.log files are left untouched.
func (cl *ContentLogger) cleanupOldFiles() {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return
	}

	type fileEntry struct {
		name    string
		modTime time.Time
	}

	var logFiles []fileEntry
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !contentLogFileRe.MatchString(name) {
			continue
		}
		if name == cl.currentName {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		logFiles = append(logFiles, fileEntry{name: name, modTime: info.ModTime()})
	}

	sort.Slice(logFiles, func(i, j int) bool {
		return logFiles[i].name < logFiles[j].name
	})

	now := time.Now()
	toCompressSet := make(map[string]bool)

	// Rule 1: Files older than CompressAgeDays → compress
	for _, f := range logFiles {
		if now.Sub(f.modTime) > time.Duration(CompressAgeDays)*24*time.Hour {
			toCompressSet[f.name] = true
		}
	}

	// Rule 2: More than KeepUncompressedCount uncompressed files → compress oldest
	remaining := len(logFiles) - len(toCompressSet)
	if remaining > KeepUncompressedCount {
		extra := remaining - KeepUncompressedCount
		count := 0
		for _, f := range logFiles {
			if toCompressSet[f.name] {
				continue
			}
			toCompressSet[f.name] = true
			count++
			if count >= extra {
				break
			}
		}
	}

	for _, f := range logFiles {
		if !toCompressSet[f.name] {
			continue
		}
		srcPath := filepath.Join(cl.dir, f.name)
		dstPath := srcPath + ".gz"
		if _, err := os.Stat(dstPath); err == nil {
			_ = os.Remove(srcPath)
			continue
		}
		if err := gzipCompressFile(srcPath, dstPath); err != nil {
			common.SysError("content_logger: compress failed for " + f.name + ": " + err.Error())
			continue
		}
		_ = os.Remove(srcPath)
		common.SysLog("content_logger: compressed " + f.name)
	}
}

// gzipCompressFile compresses srcPath to dstPath using gzip.
func gzipCompressFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	gw := gzip.NewWriter(dst)
	defer gw.Close()

	_, err = io.Copy(gw, src)
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
