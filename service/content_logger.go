package service

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
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
// ContentLogger — file-based writer with rotation, zstd compression,
// and timestamp-prefixed query for fast request_id lookup.
//
// Compression codec: content logs are dominated by cross-entry repetition
// (the same system prompts, tool schemas and SSE templates appear in
// thousands of entries), which gzip's 32KB sliding window cannot exploit —
// measured ~26% ratio on production files. zstd with a multi-MB window
// reaches <1% at speeds that make archival nearly free (100MB file in <1s),
// so rotated files are archived as .log.zst. Legacy .log.gz and .log.zip
// archives remain fully readable through the same query path.
// ---------------------------------------------------------------------------

const (
	// ContentLogDir is the new storage directory for content logs.
	ContentLogDir = "/root/share/ops/logs/new-api"
	// OldContentLogDir is the pre-migration directory; files are moved to ContentLogDir at startup.
	OldContentLogDir = "/data/content-logs"
	// ContentLogFileSuffix is the extension for active (uncompressed) log files.
	ContentLogFileSuffix = ".log"
	// ContentLogGzipSuffix is the extension for gzip-compressed log files (legacy archives, still queryable).
	ContentLogGzipSuffix = ".log.gz"
	// ContentLogZstdSuffix is the extension for zstd-compressed log files (current archive format).
	ContentLogZstdSuffix = ".log.zst"
	// OldContentLogFilePrefix is the filename prefix used by the legacy naming convention.
	OldContentLogFilePrefix = "content_"
	// OldContentLogZipSuffix is the extension for legacy zip-compressed log files.
	OldContentLogZipSuffix = ".log.zip"
	// DefaultMaxSizeMB is the default maximum size of a single log file before rotation.
	DefaultMaxSizeMB = 100
	// KeepUncompressedCount is the maximum number of uncompressed .log files to retain.
	KeepUncompressedCount = 5
	// CompressAgeDays is the age threshold (in days) beyond which uncompressed files are compressed.
	CompressAgeDays = 7
)

// contentLogFileTimeFormat is the Go time format used for filenames (UTC).
// Produces filenames like: 20260721-143050.123.log
const contentLogFileTimeFormat = "20060102-150405.000"

// contentLogFileRe matches new-format filenames: YYYYMMDD-HHmmss.SSS.log
var contentLogFileRe = regexp.MustCompile(`^\d{8}-\d{6}\.\d{3}\.log$`)

// Candidate file kinds for queryByTimestamp.
const (
	candLog = iota
	candGzip
	candZstd
)

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

	// 兜底拦截:任何分支若最终记录的响应体仍是原始 SSE 流(未合并),
	// 统一在此合并为单条聚合 JSON,保证 content log 永不记录原始 chunk 流。
	// 已合并的聚合 JSON 与非流式 JSON 不含 `data:` 帧,不会被误判。
	mergeRawSSEBodies(entry)

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
// Lookup strategy: extract the UTC timestamp embedded in the request_id prefix,
// narrow candidate files (.log and .log.gz) to a small window around that
// timestamp, and scan only those with grep/zgrep. If the entry is not found
// in that window, return not-found — we do not scan every archived file, which
// used to make queries hang for a long time before returning "not found".
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

	// Narrow candidate files by the timestamp embedded in the request_id and
	// scan only that small window. No whole-archive fallback.
	if reqTs, ok := extractRequestIdTimestamp(requestID); ok {
		return cl.queryByTimestamp(reqTs, prefix, requestID), nil
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
// near the request's timestamp, then scans only those. Uncompressed .log,
// gzip-archived .log.gz (legacy) and zstd-archived .log.zst files are all
// candidates (they share the same YYYYMMDD-HHmmss.SSS filename timestamp), so
// an entry that has already been rotated and compressed is still reachable.
func (cl *ContentLogger) queryByTimestamp(reqTs time.Time, prefix []byte, requestID string) *ContentLogEntry {
	entries, err := os.ReadDir(cl.dir)
	if err != nil {
		return nil
	}

	type cand struct {
		name  string
		tsKey string
		kind  int // candLog / candGzip / candZstd
	}
	var cands []cand
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case contentLogFileRe.MatchString(name):
			cands = append(cands, cand{name: name, tsKey: strings.TrimSuffix(name, ContentLogFileSuffix), kind: candLog})
		case strings.HasSuffix(name, ContentLogGzipSuffix):
			cands = append(cands, cand{name: name, tsKey: strings.TrimSuffix(name, ContentLogGzipSuffix), kind: candGzip})
		case strings.HasSuffix(name, ContentLogZstdSuffix):
			cands = append(cands, cand{name: name, tsKey: strings.TrimSuffix(name, ContentLogZstdSuffix), kind: candZstd})
		}
	}
	if len(cands) == 0 {
		return nil
	}

	sort.Slice(cands, func(i, j int) bool {
		return cands[i].tsKey < cands[j].tsKey
	})

	// The file open at request time has a timestamp <= reqTs. The entry may
	// also have spilled into the next file if rotation happened mid-request.
	// Probe a small window of candidates around the request timestamp.
	reqKey := reqTs.Format(contentLogFileTimeFormat)
	idx := sort.Search(len(cands), func(i int) bool {
		return cands[i].tsKey >= reqKey
	})
	// HA 场景下 blue/green 共享同一日志目录、各自持有当前写文件：请求所在
	// 文件的 tsKey 可能比另一写者新开的文件更早（落在 idx-2 而非 idx-1）。
	// 向前多探 2 个候选以覆盖双写者，向后保留 2 个以覆盖轮转中途溢出。
	start := idx - 3
	if start < 0 {
		start = 0
	}
	end := idx + 2
	if end > len(cands) {
		end = len(cands)
	}

	for i := start; i < end; i++ {
		fpath := filepath.Join(cl.dir, cands[i].name)
		var entry *ContentLogEntry
		switch cands[i].kind {
		case candGzip:
			entry = scanGzipFileForRequestID(fpath, prefix, requestID)
		case candZstd:
			entry = scanZstdFileForRequestID(fpath, prefix, requestID)
		default:
			entry = scanFileForRequestID(fpath, prefix, requestID)
		}
		if entry != nil {
			return entry
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// file scanning helpers
// ---------------------------------------------------------------------------

// parseContentLogEntry parses grep/zgrep output (one or more matched lines)
// and returns the first line whose request_id matches exactly. grep/zgrep
// print every line containing the fixed prefix; we still verify the parsed
// request_id because the prefix may also occur inside a captured response body.
func parseContentLogEntry(out []byte, requestID string) *ContentLogEntry {
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		if len(line) == 0 {
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

// scanFileForRequestID searches an uncompressed .log file with `grep -ahF`.
// grep has no per-line length limit, so it tolerates oversized log lines: a
// single multi-MB line would abort a bufio.Scanner (capped at 10MB) and make
// the target entry unfindable even though it exists further down the file.
// The -a (--text) flag is required for robustness: a single entry corrupted
// with raw NUL bytes (observed as a torn 4KB write on the vboxsf-backed log
// share) makes GNU grep classify the whole file as binary, after which it
// reports "binary file matches" and prints NO matched lines — every lookup
// against that file would return not-found. With -a the matched lines are
// always printed; a corrupted entry line simply fails JSON parsing inside
// parseContentLogEntry and is skipped.
func scanFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	out, err := exec.Command("grep", "-ahF", string(prefix), path).Output()
	if err != nil {
		return nil // grep exits non-zero when no line matched
	}
	return parseContentLogEntry(out, requestID)
}

// scanGzipFileForRequestID searches a gzip-compressed .log.gz file with
// `zgrep -ahF`, which decompresses and greps in one pass with no per-line
// limit. -a keeps the scan immune to binary-poisoned archives, matching
// scanFileForRequestID.
func scanGzipFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	out, err := exec.Command("zgrep", "-ahF", string(prefix), path).Output()
	if err != nil {
		return nil
	}
	return parseContentLogEntry(out, requestID)
}

// scanZstdFileForRequestID searches a zstd-compressed .log.zst archive.
// Decompression and line matching run in-process (klauspost/compress, ~2GB/s)
// instead of shelling out to zstdgrep — no external binary dependency at query
// time. Line scanning tolerates arbitrarily long lines (multi-MB captured
// bodies must not abort the scan) and matches raw NUL-poisoned bytes like the
// grep -a scans.
func scanZstdFileForRequestID(path string, prefix []byte, requestID string) *ContentLogEntry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	dec, err := zstd.NewReader(f)
	if err != nil {
		return nil
	}
	defer dec.Close()

	return scanReaderForRequestID(dec, prefix, requestID)
}

// scanReaderForRequestID scans an uncompressed byte stream line by line for
// an entry whose request_id matches exactly. bufio.Reader.ReadBytes grows to
// any line length, so oversized entries cannot abort the scan the way a fixed
// bufio.Scanner buffer would.
func scanReaderForRequestID(r io.Reader, prefix []byte, requestID string) *ContentLogEntry {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, prefix) {
			if entry := parseContentLogEntry(line, requestID); entry != nil {
				return entry
			}
		}
		if err != nil {
			return nil
		}
	}
}

// aggregateStreamBuffer merges a raw SSE stream buffer (captured by
// streamTrackingBody during upstream consumption) into a single aggregated
// OpenAI chat-completion JSON — the same shape reconstructOaiStreamResponse
// produces. This is the safety net for streaming responses whose handler did
// not set a reconstructed body on _content_upstream_resp. Without it the raw
// SSE stream (potentially many MB of `data:` chunks) would be logged verbatim,
// producing oversized log lines that broke the legacy request_id lookup and
// bloat storage. The user-visible rule this enforces: streaming responses are
// always recorded as one merged body after the response completes, never as the
// raw chunk stream.
//
// Only OpenAI chat-completion chunks (`choices[].delta`) are aggregated into
// structured output; anything else (Claude `event:` frames, Gemini, or
// unparseable bytes) collapses to a compact summary so the line stays bounded
// regardless of upstream format.
func aggregateStreamBuffer(raw []byte) string {
	var id, model, finishReason string
	var created int64
	var content, reasoning strings.Builder
	var toolArgs []string
	var lastUsage any
	validChunks := 0

	for _, line := range bytes.Split(raw, []byte("\n")) {
		payload := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(payload, []byte("data:")):
			payload = bytes.TrimSpace(payload[len("data:"):])
		case bytes.HasPrefix(payload, []byte(":")), bytes.HasPrefix(payload, []byte("event:")):
			continue // SSE comment / event marker (e.g. Claude frames)
		}
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}

		var chunk map[string]any
		if err := common.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if u, ok := chunk["usage"]; ok {
			lastUsage = u
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		validChunks++
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["model"].(string); ok && model == "" {
			model = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = int64(v)
		}
		first, _ := choices[0].(map[string]any)
		if fr, ok := first["finish_reason"].(string); ok && fr != "" {
			finishReason = fr
		}
		delta, ok := first["delta"].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			content.WriteString(c)
		}
		if r, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(r)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tc := range tcs {
				if m, ok := tc.(map[string]any); ok {
					if fn, ok := m["function"].(map[string]any); ok {
						if args, ok := fn["arguments"].(string); ok {
							toolArgs = append(toolArgs, args)
						}
					}
				}
			}
		}
	}

	if validChunks == 0 {
		return streamBufferSummary(raw)
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	result := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content.String()},
				"finish_reason": finishReason,
			},
		},
		"_aggregated_from_stream": true,
	}
	if reasoning.Len() > 0 {
		result["reasoning_content"] = reasoning.String()
	}
	if len(toolArgs) > 0 {
		result["tool_call_arguments"] = toolArgs
	}
	if lastUsage != nil {
		result["usage"] = lastUsage
	}
	body, err := common.Marshal(result)
	if err != nil {
		return streamBufferSummary(raw)
	}
	return string(body)
}

// streamBufferSummary is the last-resort fallback when the SSE stream is not
// OpenAI chat-completion format: it returns a small JSON describing the buffer
// (chunk count, byte size, a short preview) so the log line stays bounded
// instead of embedding the whole raw stream.
func streamBufferSummary(raw []byte) string {
	chunks := 0
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			chunks++
		}
	}
	preview := string(raw)
	if len(preview) > 500 {
		preview = preview[:500]
	}
	summary := map[string]any{
		"_stream_summary": true,
		"note":            "stream not aggregated (non-OpenAI format or unparseable)",
		"data_chunks":     chunks,
		"raw_bytes":       len(raw),
		"preview":         preview,
	}
	body, err := common.Marshal(summary)
	if err != nil {
		return fmt.Sprintf(`{"_stream_summary":true,"raw_bytes":%d}`, len(raw))
	}
	return string(body)
}

// isRawSSEStream reports whether body looks like an unaggregated SSE chunk
// stream — i.e. it contains at least one `data:` frame line. Already-aggregated
// JSON (carrying `_aggregated_from_stream`) and ordinary non-stream JSON bodies
// have no `data:` frame lines, so they are never misclassified here.
func isRawSSEStream(body string) bool {
	if body == "" {
		return false
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			return true
		}
	}
	return false
}

// mergeRawSSEBodies is the last-resort safety net run right before a content
// log entry is persisted. If any captured response body is still a raw SSE
// chunk stream (because the streaming branch was not detected earlier, a
// retry overwrote the reconstructed body, or a reconstruct helper did not set
// a body), it is merged into one aggregated JSON here. This guarantees the
// user-visible rule: streaming responses are always recorded merged, never as
// the raw chunk stream, on every branch — no matter which capture path ran.
func mergeRawSSEBodies(entry *ContentLogEntry) {
	if entry == nil {
		return
	}
	if entry.GatewayResponse != nil && isRawSSEStream(entry.GatewayResponse.Body) {
		entry.GatewayResponse.Body = aggregateStreamBuffer([]byte(entry.GatewayResponse.Body))
	}
	if entry.UpstreamResponse != nil && isRawSSEStream(entry.UpstreamResponse.Body) {
		entry.UpstreamResponse.Body = aggregateStreamBuffer([]byte(entry.UpstreamResponse.Body))
	}
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

// cleanupOldFiles zstd-compresses and removes old uncompressed .log files:
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
		dstPath := srcPath + ".zst" // == ContentLogZstdSuffix, since srcPath ends in .log
		if _, err := os.Stat(dstPath); err == nil {
			_ = os.Remove(srcPath)
			continue
		}
		if err := zstdCompressFile(srcPath, dstPath); err != nil {
			common.SysError("content_logger: compress failed for " + f.name + ": " + err.Error())
			continue
		}
		_ = os.Remove(srcPath)
		common.SysLog("content_logger: compressed " + f.name)
	}
}

// zstdCompressFile compresses srcPath to dstPath with zstd SpeedBestCompression.
// Content logs carry massive cross-entry repetition; measured on production
// files this yields <1% of the original size where the previous gzip-6 archive
// held ~26%, and a 100MB file compresses in well under a second.
func zstdCompressFile(srcPath, dstPath string) error {
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

	zw, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return dst.Close()
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
