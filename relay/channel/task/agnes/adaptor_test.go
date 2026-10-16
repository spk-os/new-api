package agnes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTaskResult(t *testing.T) {
	a := &TaskAdaptor{}

	t.Run("completed returns success with direct url", func(t *testing.T) {
		body := []byte(`{"id":"task_1","video_id":"video_1","object":"video","status":"completed","progress":100,"seconds":"4","size":"720P","url":"https://cdn.example.com/v.mp4","error":null}`)
		info, err := a.ParseTaskResult(body)
		require.NoError(t, err)
		assert.Equal(t, "SUCCESS", string(info.Status))
		assert.Equal(t, "https://cdn.example.com/v.mp4", info.Url)
		assert.Empty(t, info.Reason)
	})

	t.Run("in_progress maps without url", func(t *testing.T) {
		body := []byte(`{"id":"task_1","status":"in_progress","progress":42}`)
		info, err := a.ParseTaskResult(body)
		require.NoError(t, err)
		assert.Equal(t, "IN_PROGRESS", string(info.Status))
		assert.Equal(t, "42%", info.Progress)
		assert.Empty(t, info.Url)
	})

	t.Run("queued maps to queued", func(t *testing.T) {
		body := []byte(`{"id":"task_1","status":"queued","progress":0}`)
		info, err := a.ParseTaskResult(body)
		require.NoError(t, err)
		assert.Equal(t, "QUEUED", string(info.Status))
	})

	t.Run("failed carries upstream message", func(t *testing.T) {
		body := []byte(`{"id":"task_1","status":"failed","progress":100,"error":{"message":"Invalid reference media"}}`)
		info, err := a.ParseTaskResult(body)
		require.NoError(t, err)
		assert.Equal(t, "FAILURE", string(info.Status))
		assert.Equal(t, "Invalid reference media", info.Reason)
	})
}

func TestEstimateBilling(t *testing.T) {
	a := &TaskAdaptor{}

	tests := []struct {
		name        string
		seconds     string
		size        string
		wantSeconds float64
		wantSize    float64
	}{
		{"default seconds and 720P", "", "720P", 5, 1},
		{"string seconds 4", "4", "720P", 4, 1},
		{"1080P ratio", "6", "1080P", 6, 1.6},
		{"1K ratio", "6", "1K", 6, 1.6},
		{"2K ratio", "6", "2K", 6, 2.2},
		// seconds 是计费乘数，必须钳制到文档上限 12
		{"oversized seconds clamped", "999999", "720P", 12, 1},
		{"negative seconds falls to default", "-3", "720P", 5, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTaskRequestContext(tt.seconds, tt.size)
			ratios := a.EstimateBilling(c, &relaycommon.RelayInfo{})
			require.NotNil(t, ratios)
			assert.Equal(t, tt.wantSeconds, ratios["seconds"])
			assert.InDelta(t, tt.wantSize, ratios["size"], 1e-9)
		})
	}
}

func TestFetchTaskURL(t *testing.T) {
	var gotPath, gotVideoID, gotModelName, gotAuth string
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVideoID = r.URL.Query().Get("video_id")
		gotModelName = r.URL.Query().Get("model_name")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"task_1","status":"in_progress"}`)
		close(done)
	}))
	defer server.Close()

	a := &TaskAdaptor{}
	resp, err := a.FetchTask(server.URL, "sk-test", map[string]any{
		"task_id": "video_abc",
		"model":   "agnes-video-2.5-flash",
	}, "")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "/agnesapi", gotPath)
	assert.Equal(t, "video_abc", gotVideoID)
	assert.Equal(t, "agnes-video-2.5-flash", gotModelName)
	assert.Equal(t, "Bearer sk-test", gotAuth)
}

func newTaskRequestContext(seconds, size string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Set("task_request", relaycommon.TaskSubmitReq{
		Model:   "agnes-video-2.5-flash",
		Seconds: seconds,
		Size:    size,
	})
	return c
}
