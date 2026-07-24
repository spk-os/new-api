package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gopkg.in/yaml.v3"
)

const (
	OptGatewayRoutingEnabled    = "GatewayRoutingEnabled"
	OptGatewayRoutingConfigPath = "GatewayRoutingConfigPath"
	OptGatewayRoutingConfigYaml = "GatewayRoutingConfigYaml"
	OptGatewayAffinityEnabled   = "GatewayAffinityEnabled"
	OptGatewayChannelAutoSync   = "GatewayChannelAutoSync"
	OptGatewayCostSyncEnabled   = "GatewayCostSyncEnabled"
)

const defaultConfigPath = "/data/gateway-routing.yaml"

var (
	allowedOverLimitStrategy = map[string]bool{"queue": true, "downgrade": true, "reject": true}
	allowedFailoverPriority  = map[string]bool{"sameModel": true, "sameProvider": true}
	allowedRoutingStrategy   = map[string]bool{"priority": true, "cost": true}
)

var (
	dbReadOption  func(key string) (string, bool)
	dbWriteOption func(key, value string) error
	syncChannels  func(cfg *GatewayYaml) (*SyncResult, error)
	syncCost      func(cfg *GatewayYaml) (*CostSyncResult, error)
)

var (
	globalConfig atomic.Pointer[GatewayYaml]
	configPath   = defaultConfigPath
	configMu     sync.RWMutex
	watcherStop  chan struct{}
	watcherWG    sync.WaitGroup
	lastFileMod  time.Time
)

func SetConfigPath(p string) {
	configMu.Lock()
	defer configMu.Unlock()
	if p != "" {
		configPath = p
	}
}

func GetConfigPath() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configPath
}

func SetDBHooks(read func(string) (string, bool), write func(string, string) error) {
	dbReadOption = read
	dbWriteOption = write
}

func SetSyncHooks(channels func(*GatewayYaml) (*SyncResult, error), cost func(*GatewayYaml) (*CostSyncResult, error)) {
	syncChannels = channels
	syncCost = cost
}

func GetConfig() *GatewayYaml {
	return globalConfig.Load()
}

func swapGlobalConfig(cfg *GatewayYaml) {
	globalConfig.Store(cfg)
}

func SetConfig(cfg *GatewayYaml) {
	globalConfig.Store(cfg)
}

func ParseAndValidate(yamlText string) (*GatewayYaml, error) {
	if strings.TrimSpace(yamlText) == "" {
		return nil, errors.New("gateway config: empty YAML")
	}

	normalized := normalizeYAMLText(yamlText)

	var cfg GatewayYaml
	if err := yaml.Unmarshal([]byte(normalized), &cfg); err != nil {
		return nil, fmt.Errorf("gateway config: yaml parse: %w", err)
	}

	normalizeConfig(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func normalizeYAMLText(s string) string {
	r := strings.NewReplacer(
		"payed", "paid",
		"samepPovider", "sameProvider",
		"sameprovider", "sameProvider",
	)
	return r.Replace(s)
}

func normalizeConfig(cfg *GatewayYaml) {
	for _, group := range cfg.LLMGateway {
		if group == nil {
			continue
		}
		if group.Affinity != nil && group.Affinity.Failover != nil {
			switch strings.ToLower(group.Affinity.Failover.Priority) {
			case "sameprovider":
				group.Affinity.Failover.Priority = "sameProvider"
			case "samemodel":
				group.Affinity.Failover.Priority = "sameModel"
			}
		}
	}
	for _, prov := range cfg.Providers {
		if prov == nil {
			continue
		}
		for _, mg := range prov.Models {
			if mg == nil {
				continue
			}
			mg.Name = strings.ReplaceAll(mg.Name, "payed", "paid")
			for i, t := range mg.Tags {
				mg.Tags[i] = strings.ReplaceAll(t, "payed", "paid")
			}
		}
	}
}

func validate(cfg *GatewayYaml) error {
	if len(cfg.LLMGateway) == 0 {
		return errors.New("gateway config: llm_gateway is required")
	}
	def, ok := cfg.LLMGateway["default"]
	if !ok || def == nil {
		return errors.New("gateway config: llm_gateway.default group is required")
	}
	if strings.TrimSpace(def.Version) == "" {
		return errors.New("gateway config: llm_gateway.default.version is required")
	}
	if def.Enabled == nil {
		return errors.New("gateway config: llm_gateway.default.enabled is required")
	}

	for name, group := range cfg.LLMGateway {
		if group == nil {
			return fmt.Errorf("gateway config: llm_gateway.%s is null", name)
		}
		if err := validateGroup(name, group); err != nil {
			return err
		}
	}

	if len(cfg.Providers) == 0 {
		return errors.New("gateway config: providers is required")
	}
	for id, prov := range cfg.Providers {
		if prov == nil {
			return fmt.Errorf("gateway config: providers.%s is null", id)
		}
		if strings.TrimSpace(prov.Name) == "" {
			return fmt.Errorf("gateway config: providers.%s.name is required", id)
		}
		if strings.TrimSpace(prov.URL) == "" {
			return fmt.Errorf("gateway config: providers.%s.url is required", id)
		}
		if strings.TrimSpace(prov.Key) == "" {
			return fmt.Errorf("gateway config: providers.%s.key is required", id)
		}
		if prov.RateLimiting != nil {
			if err := validateRateLimiting(fmt.Sprintf("providers.%s", id), prov.RateLimiting); err != nil {
				return err
			}
		}
		for i, mg := range prov.Models {
			if mg == nil {
				return fmt.Errorf("gateway config: providers.%s.models[%d] is null", id, i)
			}
			if strings.TrimSpace(mg.Name) == "" {
				return fmt.Errorf("gateway config: providers.%s.models[%d].name is required", id, i)
			}
			if strings.TrimSpace(mg.Model) == "" {
				return fmt.Errorf("gateway config: providers.%s.models[%s].model is required", id, mg.Name)
			}
			if mg.Order <= 0 {
				return fmt.Errorf("gateway config: providers.%s.models[%s].order must be a positive integer", id, mg.Name)
			}
			if mg.RateLimiting != nil {
				if err := validateRateLimiting(fmt.Sprintf("providers.%s.models[%s]", id, mg.Name), mg.RateLimiting); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateGroup(name string, g *StrategyGroup) error {
	if g.LLMCommon != nil && g.LLMCommon.RateLimiting != nil {
		if err := validateRateLimiting(fmt.Sprintf("llm_gateway.%s.llmCommon.rateLimiting", name), g.LLMCommon.RateLimiting); err != nil {
			return err
		}
	}
	if g.LLMCommon != nil && g.LLMCommon.ProviderRetry != nil && g.LLMCommon.ProviderRetry.Enabled {
		mr := g.LLMCommon.ProviderRetry.Model
		if mr == nil || len(mr.BackoffIntervals) == 0 {
			return fmt.Errorf("gateway config: llm_gateway.%s.llmCommon.providerRetry.model.backoffIntervals must be non-empty when retry is enabled", name)
		}
	}
	if g.Affinity != nil {
		if g.Affinity.Enabled {
			if g.Affinity.Binding == nil || g.Affinity.Binding.TTL <= 0 {
				return fmt.Errorf("gateway config: llm_gateway.%s.affinity.binding.ttl must be > 0 when affinity is enabled", name)
			}
		}
		if g.Affinity.Failover != nil && strings.TrimSpace(g.Affinity.Failover.Priority) != "" {
			if !allowedFailoverPriority[g.Affinity.Failover.Priority] {
				return fmt.Errorf("gateway config: llm_gateway.%s.affinity.failover.priority must be one of [sameModel, sameProvider], got %q", name, g.Affinity.Failover.Priority)
			}
		}
	}
	if g.Routing != nil && g.Routing.ModelSelection != nil && strings.TrimSpace(g.Routing.ModelSelection.Strategy) != "" {
		if !allowedRoutingStrategy[g.Routing.ModelSelection.Strategy] {
			return fmt.Errorf("gateway config: llm_gateway.%s.routing.modelSelection.strategy must be one of [priority, cost], got %q", name, g.Routing.ModelSelection.Strategy)
		}
	}
	return nil
}

func validateRateLimiting(scope string, rl *RateLimiting) error {
	if !rl.Enabled {
		return nil
	}
	if strings.TrimSpace(rl.OverLimitStrategy) != "" && !allowedOverLimitStrategy[rl.OverLimitStrategy] {
		return fmt.Errorf("gateway config: %s.overLimitStrategy must be one of [queue, downgrade, reject], got %q", scope, rl.OverLimitStrategy)
	}
	return nil
}

func LoadConfig() (*GatewayYaml, error) {
	if dbReadOption != nil {
		if val, ok := dbReadOption(OptGatewayRoutingConfigYaml); ok && strings.TrimSpace(val) != "" {
			cfg, err := ParseAndValidate(val)
			if err != nil {
				return nil, fmt.Errorf("gateway config from DB: %w", err)
			}
			return cfg, nil
		}
		if val, ok := dbReadOption(OptGatewayRoutingConfigPath); ok && strings.TrimSpace(val) != "" {
			SetConfigPath(val)
		}
	}

	path := GetConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("gateway config: read %s: %w", path, err)
	}
	cfg, err := ParseAndValidate(string(data))
	if err != nil {
		return nil, fmt.Errorf("gateway config from %s: %w", path, err)
	}
	if fi, statErr := os.Stat(path); statErr == nil {
		configMu.Lock()
		lastFileMod = fi.ModTime()
		configMu.Unlock()
	}
	return cfg, nil
}

func SaveConfig(yamlText string) (*ApplyResult, error) {
	cfg, err := ParseAndValidate(yamlText)
	if err != nil {
		return nil, err
	}

	if dbWriteOption != nil {
		if err := dbWriteOption(OptGatewayRoutingConfigYaml, yamlText); err != nil {
			return nil, fmt.Errorf("gateway config: persist to DB: %w", err)
		}
	}

	path := GetConfigPath()
	if path != "" {
		if err := writeFileAtomic(path, []byte(yamlText)); err != nil {
			return nil, fmt.Errorf("gateway config: write %s: %w", path, err)
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			configMu.Lock()
			lastFileMod = fi.ModTime()
			configMu.Unlock()
		}
	}

	swapGlobalConfig(cfg)

	result := &ApplyResult{
		Applied:     true,
		EffectiveAt: time.Now().UTC().Format(time.RFC3339),
	}

	if syncChannels != nil {
		if sr, err := syncChannels(cfg); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("channel sync: %v", err))
		} else if sr != nil {
			result.ChannelsCreated = sr.Created
			result.ChannelsUpdated = sr.Updated
			result.ChannelsDisabled = sr.Disabled
		}
	}
	if syncCost != nil {
		if cr, err := syncCost(cfg); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("cost sync: %v", err))
		} else if cr != nil {
			result.PricingUpdated = cr.Updated
		}
	}
	return result, nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".gateway-*.yaml.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func HotReloadConfig() error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	swapGlobalConfig(cfg)
	return nil
}

func InitGatewayConfig() {
	if common.GatewayConfigReloadHook == nil {
		common.GatewayConfigReloadHook = func(yamlText string) error {
			cfg, err := ParseAndValidate(yamlText)
			if err != nil {
				return err
			}
			swapGlobalConfig(cfg)
			return nil
		}
	}
	// Wire up the client ID resolver so model/log.go can map User-Agent
	// strings to friendly client names without importing gateway directly.
	if common.ClientIDResolver == nil {
		common.ClientIDResolver = ResolveClientID
	}
	cfg, err := LoadConfig()
	if err != nil {
		common.SysError("gateway config init: " + err.Error())
		return
	}
	if cfg != nil {
		swapGlobalConfig(cfg)
	}
	startFileWatcher()
}

func StopGatewayConfigWatcher() {
	configMu.Lock()
	if watcherStop != nil {
		close(watcherStop)
		watcherStop = nil
	}
	configMu.Unlock()
	watcherWG.Wait()
}

func startFileWatcher() {
	configMu.Lock()
	if watcherStop != nil {
		configMu.Unlock()
		return
	}
	stop := make(chan struct{})
	watcherStop = stop
	configMu.Unlock()

	watcherWG.Add(1)
	go func() {
		defer watcherWG.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				path := GetConfigPath()
				if path == "" {
					continue
				}
				fi, err := os.Stat(path)
				if err != nil {
					continue
				}
				configMu.RLock()
				prev := lastFileMod
				configMu.RUnlock()
				if !fi.ModTime().After(prev) {
					continue
				}
				if err := HotReloadConfig(); err != nil {
					common.SysError("gateway config hot reload: " + err.Error())
					continue
				}
				configMu.Lock()
				lastFileMod = fi.ModTime()
				configMu.Unlock()
			}
		}
	}()
}

func HandleGatewayOption(key, value string) bool {
	switch key {
	case OptGatewayRoutingConfigPath:
		SetConfigPath(value)
		return true
	case OptGatewayRoutingConfigYaml:
		if strings.TrimSpace(value) == "" {
			return true
		}
		cfg, err := ParseAndValidate(value)
		if err != nil {
			common.SysError("gateway config option update: " + err.Error())
			return true
		}
		swapGlobalConfig(cfg)
		return true
	case OptGatewayRoutingEnabled,
		OptGatewayAffinityEnabled,
		OptGatewayChannelAutoSync,
		OptGatewayCostSyncEnabled:
		return true
	}
	return false
}

func RegisterGatewayOptionHooks() {}

func HandleGatewayOptionUpdate(yamlText string) error {
	if strings.TrimSpace(yamlText) == "" {
		return nil
	}
	cfg, err := ParseAndValidate(yamlText)
	if err != nil {
		return err
	}
	swapGlobalConfig(cfg)
	return nil
}
