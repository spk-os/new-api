package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const reverseSyncBaseYAML = `# gateway routing config
llm_gateway:
  default:
    version: "1.0"
    enabled: true

providers:
  alpha:
    name: alpha
    url: https://alpha.example.com
    key: key-a1,key-a2
    channelType: 1
    order: 10
    enabled: true
    models:
      # main group comment
      - name: main
        model: m1,m2
        order: 1
        enabled: true
  beta:
    name: beta
    url: https://beta.example.com
    key: key-b
    channelType: 1
    order: 5
    enabled: false
    models:
      - name: misc
        model: m3
        order: 2
        enabled: true
`

func pstr(s string) *string { return &s }

func pint64(v int64) *int64 { return &v }

// setupReverseSyncTest writes baseYAML to a temp config file and clears DB
// hooks so currentYamlText reads the file and PersistYamlText rewrites it.
func setupReverseSyncTest(t *testing.T, baseYAML string) string {
	t.Helper()
	resetGlobalState(t)
	path := filepath.Join(t.TempDir(), "gateway-routing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML), 0o644))
	SetConfigPath(path)
	return path
}

func readReverseSyncYAML(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func providerNodeIn(t *testing.T, yamlText, key string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &doc))
	root := doc.Content[0]
	providers := mappingValue(root, "providers")
	require.NotNil(t, providers)
	for i := 0; i+1 < len(providers.Content); i += 2 {
		if providers.Content[i].Value == key {
			return providers.Content[i+1]
		}
	}
	t.Fatalf("provider %q not found in yaml", key)
	return nil
}

func scalarIn(t *testing.T, mapping *yaml.Node, key string) string {
	t.Helper()
	v := mappingValue(mapping, key)
	require.NotNil(t, v, "key %q not found", key)
	return v.Value
}

func managedChannel() *model.Channel {
	return &model.Channel{
		Name:     "alpha",
		Type:     25,
		Key:      "newk1\nnewk2",
		Status:   common.ChannelStatusEnabled,
		Models:   "m1,m4",
		BaseURL:  pstr("https://alpha2.example.com"),
		Priority: pint64(7),
		Tag:      pstr(GatewayTag),
	}
}

func TestReverseSyncChannel_UpdateExistingPreservesRest(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	err := ReverseSyncChannel(managedChannel(), "")
	require.NoError(t, err)

	out := readReverseSyncYAML(t, path)
	cfg, err := ParseAndValidate(out)
	require.NoError(t, err, "reverse-synced yaml must still validate")
	require.NotNil(t, GetConfig(), "persisted config must be swapped into memory")
	assert.Equal(t, "alpha", cfg.Providers["alpha"].Name)

	prov := providerNodeIn(t, out, "alpha")
	assert.Equal(t, "newk1,newk2", scalarIn(t, prov, "key"), "multi-key channel key joins as CSV")
	assert.Equal(t, "https://alpha2.example.com", scalarIn(t, prov, "url"))
	assert.Equal(t, "25", scalarIn(t, prov, "channelType"))
	assert.Equal(t, "7", scalarIn(t, prov, "order"))
	assert.Equal(t, "true", scalarIn(t, prov, "enabled"))

	// models reconciliation: m2 dropped, m4 appended into the kept group
	groups := mappingValue(prov, "models")
	require.Len(t, groups.Content, 1)
	assert.Equal(t, "main", scalarIn(t, groups.Content[0], "name"))
	assert.Equal(t, "m1,m4", scalarIn(t, groups.Content[0], "model"))

	// untouched provider and comments survive the node-tree edit
	beta := providerNodeIn(t, out, "beta")
	assert.Equal(t, "key-b", scalarIn(t, beta, "key"))
	assert.Equal(t, "false", scalarIn(t, beta, "enabled"))
	assert.Contains(t, out, "# gateway routing config")
	assert.Contains(t, out, "# main group comment")
}

func TestReverseSyncChannel_EmptyModelsGroupRemovedAndMissingAppended(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := &model.Channel{
		Name:    "alpha",
		Status:  common.ChannelStatusEnabled,
		Key:     "key-a1",
		Models:  "m2,m9", // m1 gone from channel; m9 unknown to yaml
		BaseURL: pstr("https://alpha.example.com"),
	}
	err := ReverseSyncChannel(ch, "")
	require.NoError(t, err)

	out := readReverseSyncYAML(t, path)
	prov := providerNodeIn(t, out, "alpha")
	groups := mappingValue(prov, "models")
	require.Len(t, groups.Content, 1, "main keeps only m2; no empty group may remain")
	assert.Equal(t, "m2,m9", scalarIn(t, groups.Content[0], "model"))

	cfg, err := ParseAndValidate(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"m2,m9"}, []string{cfg.Providers["alpha"].Models[0].Model})
}

func TestReverseSyncChannel_Delete(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	require.NoError(t, ReverseSyncChannelDelete("alpha"))
	out := readReverseSyncYAML(t, path)
	assert.NotContains(t, out, "alpha.example.com")
	assert.NotContains(t, out, "newk1")
	// other provider untouched
	prov := providerNodeIn(t, out, "beta")
	assert.Equal(t, "key-b", scalarIn(t, prov, "key"))

	cfg, err := ParseAndValidate(out)
	require.NoError(t, err)
	assert.Len(t, cfg.Providers, 1)

	// deleting a nonexistent provider is a no-op, not an error
	before := readReverseSyncYAML(t, path)
	require.NoError(t, ReverseSyncChannelDelete("ghost"))
	assert.Equal(t, before, readReverseSyncYAML(t, path))
}

func TestReverseSyncChannel_AppendNewManagedChannel(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := &model.Channel{
		Name:     "Gamma Provider",
		Type:     1,
		Status:   common.ChannelStatusEnabled,
		Key:      "gk1\ngk2",
		Models:   "g1,g2",
		BaseURL:  pstr("https://gamma.example.com"),
		Priority: pint64(3),
		Tag:      pstr(GatewayTag),
	}
	require.NoError(t, ReverseSyncChannel(ch, ""))

	out := readReverseSyncYAML(t, path)
	prov := providerNodeIn(t, out, "gamma-provider")
	assert.Equal(t, "Gamma Provider", scalarIn(t, prov, "name"))
	assert.Equal(t, "gk1,gk2", scalarIn(t, prov, "key"))
	assert.Equal(t, "3", scalarIn(t, prov, "order"))
	groups := mappingValue(prov, "models")
	require.Len(t, groups.Content, 1)
	assert.Equal(t, "default", scalarIn(t, groups.Content[0], "name"))
	assert.Equal(t, "g1,g2", scalarIn(t, groups.Content[0], "model"))
	assert.Equal(t, "1", scalarIn(t, groups.Content[0], "order"), "new model group order must be positive to pass validate")

	// a second channel with the same slug gets a free key, not a collision
	ch.Name = "Gamma Provider 2"
	require.NoError(t, ReverseSyncChannel(ch, ""))
	out = readReverseSyncYAML(t, path)
	prov2 := providerNodeIn(t, out, "gamma-provider-2")
	assert.Equal(t, "Gamma Provider 2", scalarIn(t, prov2, "name"))

	_, err := ParseAndValidate(out)
	require.NoError(t, err)
}

func TestReverseSyncChannel_IgnoresUnmanagedUnknownChannel(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := &model.Channel{
		Name:    "unrelated",
		Status:  common.ChannelStatusEnabled,
		Key:     "k",
		Models:  "m1",
		BaseURL: pstr("https://x.example.com"),
		// no gateway tag
	}
	require.NoError(t, ReverseSyncChannel(ch, ""))
	assert.Equal(t, reverseSyncBaseYAML, readReverseSyncYAML(t, path))
}

func TestReverseSyncChannel_EmptyBaseURLKeepsURL(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := &model.Channel{
		Name:    "alpha",
		Status:  common.ChannelStatusEnabled,
		Key:     "key-a1",
		Models:  "m1",
		BaseURL: pstr("  "),
	}
	require.NoError(t, ReverseSyncChannel(ch, ""))

	prov := providerNodeIn(t, readReverseSyncYAML(t, path), "alpha")
	assert.Equal(t, "https://alpha.example.com", scalarIn(t, prov, "url"))
}

func TestReverseSyncChannel_RenameMatchesOldName(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := managedChannel()
	ch.Name = "alpha-renamed"
	require.NoError(t, ReverseSyncChannel(ch, "alpha"))

	out := readReverseSyncYAML(t, path)
	prov := providerNodeIn(t, out, "alpha")
	assert.Equal(t, "alpha-renamed", scalarIn(t, prov, "name"), "existing entry matched by oldName gets renamed in place")
	assert.Equal(t, "newk1,newk2", scalarIn(t, prov, "key"))
	assert.Equal(t, "beta", scalarIn(t, providerNodeIn(t, out, "beta"), "name"), "beta untouched")
}

func TestReverseSyncChannel_JSONCredentialKeyRoundTrips(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	cred := `{"accessToken":"a,b","refreshToken":"c\"d","uid":"u1"}`
	ch := &model.Channel{
		Name:    "alpha",
		Status:  common.ChannelStatusEnabled,
		Key:     cred,
		Models:  "m1",
		BaseURL: pstr("https://alpha.example.com"),
		Tag:     pstr(GatewayTag),
	}
	require.NoError(t, ReverseSyncChannel(ch, ""))

	// 含逗号的凭据 JSON key 以 CSV 引号形态写入 yaml key，可被 splitCSV 无损还原。
	prov := providerNodeIn(t, readReverseSyncYAML(t, path), "alpha")
	yamlKey := scalarIn(t, prov, "key")
	assert.Equal(t, csvQuoteSegment(cred), yamlKey)
	require.Len(t, splitCSV(yamlKey), 1)
	assert.Equal(t, cred, strings.Join(splitCSV(yamlKey), "\n"))
}

func TestProviderKeyFromChannelKeyQuoting(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		want    string
		wantOK  bool
		wantRaw []string // splitCSV(yamlKey) 期望还原的段
	}{
		{
			name:    "普通多行 key 原样逗号连接",
			key:     "k1\nk2",
			want:    "k1,k2",
			wantOK:  true,
			wantRaw: []string{"k1", "k2"},
		},
		{
			name:    "凭据 JSON key 引号包裹可还原",
			key:     `{"accessToken":"a,b"}`,
			want:    `"{""accessToken"":""a,b""}"`,
			wantOK:  true,
			wantRaw: []string{`{"accessToken":"a,b"}`},
		},
		{name: "空 key 拒绝", key: "  \n ", want: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := providerKeyFromChannelKey(tt.key)
			require.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
			if tt.wantRaw != nil {
				assert.Equal(t, tt.wantRaw, splitCSV(got))
			}
		})
	}
}

func TestReverseSyncChannel_NoConfigIsNoop(t *testing.T) {
	resetGlobalState(t)
	SetConfigPath(filepath.Join(t.TempDir(), "absent.yaml"))
	ch := managedChannel()
	require.NoError(t, ReverseSyncChannel(ch, ""))
	require.NoError(t, ReverseSyncChannelDelete(ch.Name))
}

func TestReverseSyncChannel_StatusConstantsMapToEnabledFlag(t *testing.T) {
	path := setupReverseSyncTest(t, reverseSyncBaseYAML)

	ch := managedChannel()
	ch.Status = common.ChannelStatusManuallyDisabled
	require.NoError(t, ReverseSyncChannel(ch, ""))
	prov := providerNodeIn(t, readReverseSyncYAML(t, path), "alpha")
	assert.Equal(t, "false", scalarIn(t, prov, "enabled"))

	ch.Status = common.ChannelStatusEnabled
	require.NoError(t, ReverseSyncChannel(ch, ""))
	prov = providerNodeIn(t, readReverseSyncYAML(t, path), "alpha")
	assert.Equal(t, "true", scalarIn(t, prov, "enabled"))

	// auto-disabled (circuit breaker) also maps to enabled: false
	ch.Status = common.ChannelStatusAutoDisabled
	require.NoError(t, ReverseSyncChannel(ch, ""))
	prov = providerNodeIn(t, readReverseSyncYAML(t, path), "alpha")
	assert.Equal(t, "false", scalarIn(t, prov, "enabled"))
}

func TestReverseSyncChannel_InvalidYamlNotPersisted(t *testing.T) {
	path := setupReverseSyncTest(t, "not: [valid: yaml")

	ch := managedChannel()
	err := ReverseSyncChannel(ch, "")
	require.Error(t, err)
	// the broken file is left as-is; nothing overwrites it
	assert.Equal(t, "not: [valid: yaml", readReverseSyncYAML(t, path))
}

func TestReverseSyncChannel_MalformedConfigNotOverwritten(t *testing.T) {
	// A yaml that parses as a document but fails gateway validate (missing
	// providers) must never be replaced by a rebuilt config.
	path := setupReverseSyncTest(t, "llm_gateway:\n  default:\n    version: \"1.0\"\n    enabled: true\n")
	require.NoError(t, ReverseSyncChannel(managedChannel(), ""))
	assert.True(t, strings.HasPrefix(readReverseSyncYAML(t, path), "llm_gateway:"))
}

func TestReverseSyncChannel_PreservesDailyTokenTiers(t *testing.T) {
	baseYAML := strings.Replace(reverseSyncBaseYAML,
		"providers:",
		"daily_token_tiers: [5000, 10000, 15000, 20000]\nproviders:", 1)
	baseYAML = strings.Replace(baseYAML,
		"  alpha:\n    name: alpha",
		"  alpha:\n    dailyTokenTiers: [1000]\n    name: alpha", 1)

	path := setupReverseSyncTest(t, baseYAML)
	require.NoError(t, ReverseSyncChannel(managedChannel(), ""))

	out := readReverseSyncYAML(t, path)
	cfg, err := ParseAndValidate(out)
	require.NoError(t, err, "reverse-synced yaml must still validate")
	assert.Equal(t, []float64{5000, 10000, 15000, 20000}, cfg.DailyTokenTiers,
		"top-level daily_token_tiers survives the node-tree edit")
	assert.Equal(t, []float64{1000}, cfg.Providers["alpha"].DailyTokenTiers,
		"provider-level dailyTokenTiers survives the node-tree edit")
}
