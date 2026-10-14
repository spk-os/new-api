package service

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mkContentEntry(rid string) *ContentLogEntry {
	return &ContentLogEntry{RequestID: rid, ModelName: "m"}
}

// requireCmd skips the test when an external scan tool is missing from PATH.
// The production runtime image (debian:bookworm-slim) ships grep and zgrep via
// essential packages; alpine-based test containers may lack zgrep.
func requireCmd(t *testing.T, cmds ...string) {
	t.Helper()
	for _, c := range cmds {
		if _, err := exec.LookPath(c); err != nil {
			t.Skipf("%s not in PATH, skipping (production image ships it)", c)
		}
	}
}

func writeContentEntries(t *testing.T, path string, entries ...*ContentLogEntry) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	for _, e := range entries {
		data, err := common.Marshal(e)
		require.NoError(t, err)
		_, err = f.Write(append(data, '\n'))
		require.NoError(t, err)
	}
}

// parseContentLogEntry must pick the line whose request_id matches exactly,
// ignoring other matched lines (e.g. the prefix appearing in a response body).
func TestParseContentLogEntry_PicksExactRequestId(t *testing.T) {
	other, _ := common.Marshal(mkContentEntry("other"))
	target, _ := common.Marshal(mkContentEntry("target"))
	out := bytes.Join([][]byte{other, target}, []byte("\n"))

	got := parseContentLogEntry(out, "target")
	require.NotNil(t, got)
	assert.Equal(t, "target", got.RequestID)

	// No matching request_id among the lines.
	assert.Nil(t, parseContentLogEntry(out, "missing"))
}

// Regression for the "content log not found after a long wait" bug:
// a bufio.Scanner capped at 10MB aborted on the 11MB line and never reached
// the target entry later in the file. grep has no per-line limit, so it must
// skip the oversized line and find the target.
func TestScanFileForRequestID_FindsEntryAfterOversizedLine(t *testing.T) {
	requireCmd(t, "grep")
	big := mkContentEntry("other")
	big.UpstreamResponse = &HttpMessage{Body: strings.Repeat("x", 11*1024*1024)}
	target := mkContentEntry("target-rid")

	dir := t.TempDir()
	path := filepath.Join(dir, "20260807-080317.516.log")
	writeContentEntries(t, path, big, target)

	prefix := []byte(`"request_id":"target-rid"`)
	got := scanFileForRequestID(path, prefix, "target-rid")
	require.NotNil(t, got)
	assert.Equal(t, "target-rid", got.RequestID)
}

// queryByTimestamp must include gzip-archived .log.gz files as candidates
// Regression for the "content log not found" caused by a binary-poisoned log
// file: one entry corrupted with raw NUL bytes (a torn 4KB write on the
// vboxsf-backed log share) makes GNU grep classify the whole file as binary
// and print "binary file matches" instead of the matched lines. scan must use
// grep -a so entries in that file remain findable.
func TestScanFileForRequestID_FindsEntryInNulPoisonedFile(t *testing.T) {
	requireCmd(t, "grep")
	poisoned, err := common.Marshal(mkContentEntry("poisoned-rid"))
	require.NoError(t, err)
	// Inject raw NUL bytes so the file is classified as binary by grep.
	poisoned = append(poisoned[:len(poisoned)-1], append(make([]byte, 4096), '}', '\n')...)
	target := mkContentEntry("target-rid")

	dir := t.TempDir()
	path := filepath.Join(dir, "20261009-093524.536.log")
	targetData, err := common.Marshal(target)
	require.NoError(t, err)
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = f.Write(append(poisoned, append(targetData, '\n')...))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	prefix := []byte(`"request_id":"target-rid"`)
	got := scanFileForRequestID(path, prefix, "target-rid")
	require.NotNil(t, got)
	assert.Equal(t, "target-rid", got.RequestID)
}

// (they share the .log filename timestamp format) and locate an entry that
// has already been rotated and compressed — without scanning every archive.
func TestQueryByTimestamp_FindsEntryInGzArchive(t *testing.T) {
	requireCmd(t, "zgrep")
	const rid = "202608070833195763817808268d9d6rtok6IP2"

	dir := t.TempDir()
	// Archive file timestamped 08:03, request at 08:33 -> falls in the window.
	path := filepath.Join(dir, "20260807-080317.516.log.gz")
	data, err := common.Marshal(mkContentEntry(rid))
	require.NoError(t, err)

	f, err := os.Create(path)
	require.NoError(t, err)
	gw := gzip.NewWriter(f)
	_, err = gw.Write(append(data, '\n'))
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	reqTs, ok := extractRequestIdTimestamp(rid)
	require.True(t, ok)

	cl := &ContentLogger{dir: dir}
	prefix := []byte(`"request_id":"` + rid + `"`)
	got := cl.queryByTimestamp(reqTs, prefix, rid)
	require.NotNil(t, got)
	assert.Equal(t, rid, got.RequestID)
}

// queryByTimestamp must include zstd-archived .log.zst files as candidates.
// Regression guard for the gzip→zstd archive switch: entries written before a
// rotation must stay findable in the new archive format, alongside legacy
// .log.gz files.
func TestQueryByTimestamp_FindsEntryInZstdArchive(t *testing.T) {
	const rid = "202608070833195763817808268d9d6rtok6IP2"

	dir := t.TempDir()
	// Archive file timestamped 08:03, request at 08:33 -> falls in the window.
	path := filepath.Join(dir, "20260807-080317.516.log.zst")
	data, err := common.Marshal(mkContentEntry(rid))
	require.NoError(t, err)

	f, err := os.Create(path)
	require.NoError(t, err)
	zw, err := zstd.NewWriter(f)
	require.NoError(t, err)
	_, err = zw.Write(append(data, '\n'))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	reqTs, ok := extractRequestIdTimestamp(rid)
	require.True(t, ok)

	cl := &ContentLogger{dir: dir}
	prefix := []byte(`"request_id":"` + rid + `"`)
	got := cl.queryByTimestamp(reqTs, prefix, rid)
	require.NotNil(t, got)
	assert.Equal(t, rid, got.RequestID)
}

// zstdCompressFile must produce an archive whose decompressed content matches
// the source byte-for-byte (the write-side counterpart of the scan tests).
func TestZstdCompressFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "20261009-051835.360.log")
	srcData := append(
		append(bytes.Repeat([]byte("system prompt repeated "), 5000), '\n'),
		append([]byte(`{"request_id":"202610091519453188774708268d9d6gDHqbxWc"}`), '\n')...,
	)
	require.NoError(t, os.WriteFile(srcPath, srcData, 0o644))

	dstPath := srcPath + ".zst"
	require.NoError(t, zstdCompressFile(srcPath, dstPath))

	raw, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	defer dec.Close()
	out, err := io.ReadAll(dec)
	require.NoError(t, err)
	assert.Equal(t, srcData, out)
	// High-repetition content must compress far below the old gzip ratio.
	assert.Less(t, len(raw), len(srcData)/20)
}

// aggregateStreamBuffer must merge a raw OpenAI chat-completion SSE stream
// into a single aggregated JSON (content + reasoning_content + tool_call
// arguments + usage), never leaving the raw `data:` chunk stream intact. This
// is the regression guard for the oversized-log-line bug: a streaming response
// must be recorded as one merged body after completion.
func TestAggregateStreamBuffer_MergesOpenAIChunks(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"think"}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":"}}]}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
		`: keepalive comment`,
		`data: [DONE]`,
		"",
	}, "\n")

	out := aggregateStreamBuffer([]byte(stream))

	var got map[string]any
	require.NoError(t, common.Unmarshal([]byte(out), &got))
	assert.Equal(t, "chatcmpl-1", got["id"])
	assert.Equal(t, "chat.completion", got["object"])
	assert.Equal(t, "deepseek-v4-flash", got["model"])
	assert.Equal(t, true, got["_aggregated_from_stream"])
	assert.Equal(t, float64(1700000000), got["created"])

	choices := got["choices"].([]any)
	first := choices[0].(map[string]any)
	assert.Equal(t, "stop", first["finish_reason"])
	msg := first["message"].(map[string]any)
	assert.Equal(t, "Hello", msg["content"])

	assert.Equal(t, "think", got["reasoning_content"])
	assert.Equal(t, []any{`{"a":`, `1}`}, got["tool_call_arguments"].([]any))

	usage := got["usage"].(map[string]any)
	assert.Equal(t, float64(5), usage["prompt_tokens"])

	// Sanity: aggregated body must be far smaller than the raw stream.
	assert.Less(t, len(out), len(stream)*2)
}

// aggregateStreamBuffer must never reproduce the raw stream. When the buffer
// is not OpenAI chat-completion format (e.g. Claude `event:` frames), it
// collapses to a bounded summary so the log line stays small regardless of
// upstream format.
func TestAggregateStreamBuffer_NonOpenAISummary(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}`,
		"",
	}, "\n")

	out := aggregateStreamBuffer([]byte(stream))

	var got map[string]any
	require.NoError(t, common.Unmarshal([]byte(out), &got))
	assert.Equal(t, true, got["_stream_summary"])
	assert.Nil(t, got["choices"]) // no fake chat-completion shape
	// Summary stays bounded even if raw grew large.
	bigRaw := append([]byte(stream), bytes.Repeat([]byte("event: ping\ndata: {\"type\":\"ping\"}\n"), 1000)...)
	bigOut := aggregateStreamBuffer(bigRaw)
	assert.Less(t, len(bigOut), 2000)
}

// queryByTimestamp must return nil when the entry is outside the candidate
// window, instead of falling back to scan the whole archive.
// TestQueryByTimestamp_FindsEntryAcrossConcurrentWriters 锁定 HA 双写者场景：
// blue/green 共享日志目录、各自持有当前写文件时，请求所在文件的 tsKey 可能
// 比另一写者新开的文件更早（位于 idx-2）。回归根因：窗口曾只向前探 idx-1，
// 导致生产环境 Content Log 详情报 "content log not found"。
func TestQueryByTimestamp_FindsEntryAcrossConcurrentWriters(t *testing.T) {
	requireCmd(t, "grep")
	const rid = "202608070833195763817808268d9d6rtok6IP2"

	dir := t.TempDir()
	// 请求 ts=08:33。并发写者的文件 tsKey 更晚（08:40）且不包含该请求；
	// 真正的入口在 tsKey 更早的 08:20 文件里。
	writeContentEntries(t, filepath.Join(dir, "20260807-082000.000.log"), mkContentEntry(rid))
	writeContentEntries(t, filepath.Join(dir, "20260807-084000.000.log"), mkContentEntry("other-writer"))

	reqTs, ok := extractRequestIdTimestamp(rid)
	require.True(t, ok)

	cl := &ContentLogger{dir: dir}
	prefix := []byte(`"request_id":"` + rid + `"`)
	got := cl.queryByTimestamp(reqTs, prefix, rid)
	require.NotNil(t, got)
	assert.Equal(t, rid, got.RequestID)
}

func TestQueryByTimestamp_ReturnsNilOutsideWindow(t *testing.T) {
	requireCmd(t, "grep")
	const rid = "202608070833195763817808268d9d6rtok6IP2"

	dir := t.TempDir()
	// A file from a much earlier day holding an unrelated entry.
	writeContentEntries(t, filepath.Join(dir, "20260801-000000.000.log"), mkContentEntry("unrelated"))

	reqTs, ok := extractRequestIdTimestamp(rid)
	require.True(t, ok)

	cl := &ContentLogger{dir: dir}
	prefix := []byte(`"request_id":"` + rid + `"`)
	assert.Nil(t, cl.queryByTimestamp(reqTs, prefix, rid))
}
