package workbuddy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// catalogConfig 是 /v3/config 响应中与模型花名册相关的字段。
// 实测响应形态：{"code":0,"msg":"OK","data":{"models":[{"id","name",...}],"agents":[{"name":"cli","models":[...]}],...}}
type catalogConfig struct {
	Data struct {
		Models []catalogModel `json:"models"`
		Agents []catalogAgent `json:"agents"`
	} `json:"data"`
}

type catalogModel struct {
	ID string `json:"id"`
}

type catalogAgent struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// reservedCatalogIDs 是 /v3/config data.models 中不代表可调用聊天模型的占位 id。
var reservedCatalogIDs = map[string]bool{
	"default": true,
}

// FetchCatalogModels 拉取 WorkBuddy 模型花名册（模型名与上游一致）。
// 优先取 agent[cli].models —— 该账号 CLI agent 实际可调用的子集；
// 缺失时回退到 data.models 全表并剔除占位 id。
func FetchCatalogModels(ctx context.Context, key, baseURL string) ([]string, error) {
	cred, err := ResolveCredential(ctx, key, baseURL)
	if err != nil {
		return nil, fmt.Errorf("workbuddy resolve credential: %w", err)
	}
	requestURL := strings.TrimSuffix(baseURL, "/") + "/v3/config"
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("User-Agent", workbuddyClientUA)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", workbuddySiteOrigin)
	req.Header.Set("Referer", workbuddySiteOrigin+"/")
	if cred.UID != "" {
		req.Header.Set("X-User-Id", cred.UID)
	}
	if cred.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", cred.EnterpriseID)
	}
	if cred.Domain != "" {
		req.Header.Set("X-Domain", cred.Domain)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("workbuddy /v3/config HTTP %d: %s", resp.StatusCode, common.GetJsonType(body))
	}
	var catalog catalogConfig
	if err := common.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("workbuddy /v3/config parse: %w", err)
	}

	for _, agent := range catalog.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		models := normalizeCatalogModels(agent.Models)
		if len(models) > 0 {
			return models, nil
		}
	}
	ids := make([]string, 0, len(catalog.Data.Models))
	for _, m := range catalog.Data.Models {
		ids = append(ids, m.ID)
	}
	return normalizeCatalogModels(ids), nil
}

// normalizeCatalogModels 去空格、去占位 id、按序去重。
func normalizeCatalogModels(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || reservedCatalogIDs[id] || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	return models
}
