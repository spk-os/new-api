package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestRewriteRequestBodyModel(t *testing.T) {
	body := `{"model":"auto-document","messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// Initialize body storage (simulates what the real middleware chain does)
	storage, err := common.CreateBodyStorage([]byte(body))
	if err != nil {
		t.Fatalf("CreateBodyStorage: %v", err)
	}
	c.Set(common.KeyBodyStorage, storage)
	c.Request.Body = io.NopCloser(storage)

	rewriteRequestBodyModel(c, "minimax-m3-free")

	newStorage, err := common.GetBodyStorage(c)
	if err != nil {
		t.Fatalf("GetBodyStorage after rewrite: %v", err)
	}
	bs, err := newStorage.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	result := gjson.GetBytes(bs, "model")
	if result.String() != "minimax-m3-free" {
		t.Errorf("expected model=minimax-m3-free, got %q", result.String())
	}

	msgResult := gjson.GetBytes(bs, "messages.0.content")
	if msgResult.String() != "hello" {
		t.Errorf("messages should be preserved, got %q", msgResult.String())
	}
}

func TestRewriteRequestBodyModel_NoModelField(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hello"}]}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	storage, err := common.CreateBodyStorage([]byte(body))
	if err != nil {
		t.Fatalf("CreateBodyStorage: %v", err)
	}
	c.Set(common.KeyBodyStorage, storage)
	c.Request.Body = io.NopCloser(storage)

	rewriteRequestBodyModel(c, "minimax-m3-free")

	newStorage, err := common.GetBodyStorage(c)
	if err != nil {
		t.Fatalf("GetBodyStorage: %v", err)
	}
	bs, err := newStorage.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	// Body should be unchanged
	if string(bs) != body {
		t.Errorf("body should be unchanged when no model field, got %q", string(bs))
	}
}
