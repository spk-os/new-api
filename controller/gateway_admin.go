package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service/gateway"
	"github.com/gin-gonic/gin"
)

func GetGatewayConfig(c *gin.Context) {
	common.OptionMapRWMutex.RLock()
	yamlText := common.OptionMap["GatewayRoutingConfigYaml"]
	enabled := common.OptionMap["GatewayRoutingEnabled"]
	configPath := common.OptionMap["GatewayRoutingConfigPath"]
	common.OptionMapRWMutex.RUnlock()

	if enabled == "" {
		enabled = "false"
	}
	if configPath == "" {
		configPath = "/data/gateway-routing.yaml"
	}
	c.JSON(http.StatusOK, gin.H{
		"yaml":        yamlText,
		"enabled":     enabled,
		"config_path": configPath,
	})
}

func SaveGatewayConfig(c *gin.Context) {
	var req struct {
		Yaml string `json:"yaml" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	result, err := gateway.SaveConfig(req.Yaml)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "line": extractGatewayErrorLine(err)})
		return
	}
	c.JSON(http.StatusOK, result)
}

func ValidateGatewayConfig(c *gin.Context) {
	var req struct {
		Yaml string `json:"yaml" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if _, err := gateway.ParseAndValidate(req.Yaml); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "line": extractGatewayErrorLine(err)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true})
}

func SyncGatewayChannels(c *gin.Context) {
	cfg := gateway.GetConfig()
	if cfg == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gateway config not loaded"})
		return
	}

	chResult, chErr := gateway.SyncChannels(cfg)
	costResult, costErr := gateway.SyncCost(cfg.Cost)

	errs := []string{}
	if chErr != nil {
		errs = append(errs, chErr.Error())
	}
	if costErr != nil {
		errs = append(errs, costErr.Error())
	}

	c.JSON(http.StatusOK, gin.H{
		"channels": chResult,
		"cost":     costResult,
		"errors":   errs,
	})
}

func PreviewGatewayRoute(c *gin.Context) {
	var req struct {
		Group    string `json:"group"`
		ClientId string `json:"client_id"`
		Model    string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	cfg := gateway.GetConfig()
	if cfg == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "gateway config not loaded"})
		return
	}

	if strings.TrimSpace(req.Group) == "" {
		req.Group = "default"
	}

	profile := gateway.ResolveEffectiveProfile(req.Group, req.ClientId, "", "")
	if profile == nil {
		profile = &gateway.EffectiveProfile{}
	}

	candidates := gateway.BuildCandidates(profile, req.Model, nil)

	strategy := "priority"
	if profile.Routing != nil && profile.Routing.ModelSelection != nil && profile.Routing.ModelSelection.Strategy != "" {
		strategy = profile.Routing.ModelSelection.Strategy
	}

	c.JSON(http.StatusOK, &gateway.RoutePreviewResult{
		Group:      req.Group,
		ClientId:   req.ClientId,
		Model:      req.Model,
		Strategy:   strategy,
		Candidates: candidates,
	})
}

func GetGatewayAffinity(c *gin.Context) {
	bindings := gateway.ListAffinityBindings()
	c.JSON(http.StatusOK, gin.H{"bindings": bindings})
}

func ClearGatewayAffinity(c *gin.Context) {
	gateway.ClearAffinityBindings()
	c.JSON(http.StatusOK, gin.H{"message": "affinity bindings cleared"})
}

func GetGatewayStats(c *gin.Context) {
	stats := &gateway.GatewayStats{
		ProviderRequests: map[string]int64{},
		ModelRequests:    map[string]int64{},
		RetryCounts:      map[string]int64{},
	}
	c.JSON(http.StatusOK, stats)
}

func extractGatewayErrorLine(err error) int {
	if err == nil {
		return -1
	}
	msg := err.Error()
	idx := strings.LastIndex(msg, "line ")
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len("line "):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return -1
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return -1
	}
	return n
}
