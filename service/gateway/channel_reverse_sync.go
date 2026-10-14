package gateway

// Reverse sync: console channel (渠道) → gateway routing YAML.
//
// Forward sync (channel_sync.go) is one-way config→DB: every SaveConfig
// overwrites the key/models/status/baseURL of any channel matching a provider
// `name`, silently reverting whatever an admin changed on the channel page.
// This file closes the loop: when a gateway-related channel is created,
// updated, enabled/disabled or deleted from the console, the matching
// `providers` entry in the YAML is patched in place and the config is
// persisted through the same path as SaveConfig (DB option + file + memory).
//
// The YAML is edited as a gopkg.in/yaml.v3 node tree so untouched sections
// (and comments on untouched nodes) survive byte-for-byte as much as the
// encoder allows. A reverse sync never runs the forward sync hooks, so the
// two directions cannot feed back into each other.

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gopkg.in/yaml.v3"
)

// reverseSyncMu serialises load→patch→persist so concurrent channel writes
// cannot interleave and lose one side's edit.
var reverseSyncMu sync.Mutex

// ReverseSyncChannel upserts the providers entry matching ch from the
// channel's current DB state. The entry is matched by provider `name`; when
// oldName is non-empty and the channel was renamed, the entry matched by
// oldName gets its name rewritten. A managed channel (tag=llm_gateway) with
// no matching entry is appended as a new provider. Channels that neither
// match a provider nor carry the gateway tag are ignored.
func ReverseSyncChannel(ch *model.Channel, oldName string) error {
	if ch == nil || strings.TrimSpace(ch.Name) == "" {
		return nil
	}
	reverseSyncMu.Lock()
	defer reverseSyncMu.Unlock()

	text, ok := currentYamlText()
	if !ok {
		// No gateway config anywhere — nothing to keep in sync.
		return nil
	}
	root, err := parseYamlDoc(text)
	if err != nil {
		return err
	}
	providers := mappingValue(root, "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return nil
	}

	idx := findProviderIndex(providers, ch.Name)
	if idx < 0 && oldName != "" && oldName != ch.Name {
		idx = findProviderIndex(providers, oldName)
	}
	if idx >= 0 {
		applyChannelToProvider(providers.Content[2*idx+1], ch)
	} else if ch.GetTag() == GatewayTag && canCreateProvider(ch) {
		key := freeProviderKey(providers, providerKeyFor(ch.Name))
		providers.Content = append(providers.Content, strScalar(key), newProviderNode(ch))
	} else {
		return nil
	}

	return persistNode(root)
}

// ReverseSyncChannelDelete removes the providers entry whose `name` matches
// the deleted channel. Entries not found are ignored.
func ReverseSyncChannelDelete(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	reverseSyncMu.Lock()
	defer reverseSyncMu.Unlock()

	text, ok := currentYamlText()
	if !ok {
		return nil
	}
	root, err := parseYamlDoc(text)
	if err != nil {
		return err
	}
	providers := mappingValue(root, "providers")
	if providers == nil || providers.Kind != yaml.MappingNode {
		return nil
	}
	idx := findProviderIndex(providers, name)
	if idx < 0 {
		return nil
	}
	providers.Content = append(providers.Content[:2*idx], providers.Content[2*idx+2:]...)
	return persistNode(root)
}

// ReverseSyncChannelsByTag reverse-syncs every channel carrying the tag from
// its current DB state. Used after tag-level operations (bulk enable/disable,
// bulk model/priority edit) that bypass single-channel handlers.
func ReverseSyncChannelsByTag(tag string) error {
	if strings.TrimSpace(tag) == "" {
		return nil
	}
	channels, err := model.GetChannelsByTag(tag, false, true)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if err := ReverseSyncChannel(ch, ""); err != nil {
			common.SysError("gateway reverse sync (tag " + tag + ", channel " + ch.Name + "): " + err.Error())
		}
	}
	return nil
}

// applyChannelToProvider patches an existing provider mapping node from the
// channel. Fields the channel cannot express (empty BaseURL/key) are left
// untouched so a partial view never blanks a valid provider.
func applyChannelToProvider(prov *yaml.Node, ch *model.Channel) {
	if prov == nil || prov.Kind != yaml.MappingNode {
		return
	}
	setScalar(prov, "name", ch.Name)
	if ch.BaseURL != nil && strings.TrimSpace(*ch.BaseURL) != "" {
		setScalar(prov, "url", strings.TrimSpace(*ch.BaseURL))
	}
	if k, ok := providerKeyFromChannelKey(ch.Key); ok {
		setScalar(prov, "key", k)
	}
	chType := ch.Type
	if chType == 0 {
		chType = 1
	}
	setInt(prov, "channelType", chType)
	if ch.Priority != nil {
		setInt(prov, "order", int(*ch.Priority))
	}
	setBool(prov, "enabled", ch.Status == common.ChannelStatusEnabled)
	reconcileProviderModels(prov, splitCSV(ch.Models))
}

// providerKeyFromChannelKey converts a channel key (newline-separated for
// multi-key channels) into the provider's comma-separated key format. It
// fails when a key part itself contains a comma — such JSON-format keys
// cannot be represented unambiguously in the provider key CSV, and the
// caller must leave the YAML key untouched instead of corrupting it.
// csvQuoteSegment wraps a key segment in CSV double quotes (escaping inner
// quotes as "") when it contains commas or quotes, so that splitCSV can
// round-trip values such as credential JSON keys through the YAML provider
// key field. Plain segments are returned unchanged.
func csvQuoteSegment(s string) string {
	if !strings.ContainsAny(s, ",\"") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func providerKeyFromChannelKey(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	parts := strings.Split(key, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, csvQuoteSegment(p))
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, ","), true
}

// reconcileProviderModels rewrites the provider's `models` groups so the
// union of group models equals desired, while preserving the existing group
// structure (names, tags, order, comments) wherever possible: models stay in
// their current group, models removed from the channel are dropped, groups
// that lose every model are removed, and unknown models are appended to the
// first remaining group (or a fresh "default" group when none is left).
func reconcileProviderModels(prov *yaml.Node, desired []string) {
	if len(desired) == 0 {
		return
	}
	desired = dedupeStrings(desired)
	desiredSet := make(map[string]bool, len(desired))
	for _, m := range desired {
		desiredSet[m] = true
	}

	modelsNode := mappingValue(prov, "models")
	if modelsNode == nil {
		modelsNode = &yaml.Node{Kind: yaml.SequenceNode}
		modelsNode.Content = []*yaml.Node{newModelGroupNode("default", desired)}
		appendNode(prov, "models", modelsNode)
		return
	}
	if modelsNode.Kind != yaml.SequenceNode || len(modelsNode.Content) == 0 {
		// Unexpected shape (scalar/flow) or empty list — replace wholesale.
		*modelsNode = yaml.Node{Kind: yaml.SequenceNode}
		modelsNode.Content = []*yaml.Node{newModelGroupNode("default", desired)}
		return
	}

	placed := make(map[string]bool, len(desired))
	kept := make([]*yaml.Node, 0, len(modelsNode.Content))
	for _, group := range modelsNode.Content {
		modelVal := mappingValue(group, "model")
		if modelVal == nil || modelVal.Kind != yaml.ScalarNode {
			kept = append(kept, group)
			continue
		}
		original := splitCSV(modelVal.Value)
		keep := make([]string, 0, len(original))
		for _, m := range original {
			if desiredSet[m] && !placed[m] {
				keep = append(keep, m)
				placed[m] = true
			}
		}
		if len(keep) == 0 {
			// Group no longer holds any channel model — drop it.
			continue
		}
		if len(keep) != len(original) {
			modelVal.Value = strings.Join(keep, ",")
		}
		kept = append(kept, group)
	}

	missing := make([]string, 0, len(desired))
	for _, m := range desired {
		if !placed[m] {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		if len(kept) > 0 {
			modelVal := mappingValue(kept[0], "model")
			if modelVal != nil && modelVal.Kind == yaml.ScalarNode {
				modelVal.Value = strings.Join(append(splitCSV(modelVal.Value), missing...), ",")
			}
		} else {
			kept = append(kept, newModelGroupNode("default", missing))
		}
	}
	modelsNode.Content = kept
}

func newModelGroupNode(name string, models []string) *yaml.Node {
	g := &yaml.Node{Kind: yaml.MappingNode}
	appendScalar(g, "name", name)
	appendScalar(g, "model", strings.Join(models, ","))
	appendInt(g, "order", 1)
	appendBool(g, "enabled", true)
	return g
}

// canCreateProvider reports whether a brand-new managed channel carries
// enough information to build a provider entry that passes validate()
// (url and key are required there).
func canCreateProvider(ch *model.Channel) bool {
	if ch.BaseURL == nil || strings.TrimSpace(*ch.BaseURL) == "" {
		return false
	}
	k, ok := providerKeyFromChannelKey(ch.Key)
	return ok && k != "" && len(splitCSV(ch.Models)) > 0
}

func newProviderNode(ch *model.Channel) *yaml.Node {
	p := &yaml.Node{Kind: yaml.MappingNode}
	appendScalar(p, "name", ch.Name)
	if ch.BaseURL != nil {
		appendScalar(p, "url", strings.TrimSpace(*ch.BaseURL))
	}
	if k, ok := providerKeyFromChannelKey(ch.Key); ok {
		appendScalar(p, "key", k)
	}
	chType := ch.Type
	if chType == 0 {
		chType = 1
	}
	appendInt(p, "channelType", chType)
	if ch.Priority != nil {
		appendInt(p, "order", int(*ch.Priority))
	}
	appendBool(p, "enabled", ch.Status == common.ChannelStatusEnabled)
	models := &yaml.Node{Kind: yaml.SequenceNode}
	models.Content = []*yaml.Node{newModelGroupNode("default", splitCSV(ch.Models))}
	appendNode(p, "models", models)
	return p
}

// providerKeyFor derives a providers map key (providerId) from a channel name.
func providerKeyFor(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "provider"
	}
	return out
}

// freeProviderKey returns key, or key-2/key-3/... when the base key is taken.
func freeProviderKey(providers *yaml.Node, key string) string {
	taken := make(map[string]bool, len(providers.Content)/2)
	for i := 0; i < len(providers.Content); i += 2 {
		taken[providers.Content[i].Value] = true
	}
	if !taken[key] {
		return key
	}
	for n := 2; ; n++ {
		candidate := key + "-" + strconv.Itoa(n)
		if !taken[candidate] {
			return candidate
		}
	}
}

func findProviderIndex(providers *yaml.Node, name string) int {
	if providers == nil || providers.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(providers.Content); i += 2 {
		nameVal := mappingValue(providers.Content[i+1], "name")
		if nameVal != nil && nameVal.Kind == yaml.ScalarNode && nameVal.Value == name {
			return i / 2
		}
	}
	return -1
}

// ============================================================
// yaml.Node helpers
// ============================================================

func parseYamlDoc(text string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("gateway reverse sync: parse yaml: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, errors.New("gateway reverse sync: empty yaml document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("gateway reverse sync: top-level yaml is not a mapping")
	}
	return root, nil
}

// persistNode re-encodes the patched tree (2-space indent, comments
// preserved) and persists it through the same path as SaveConfig.
func persistNode(root *yaml.Node) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		_ = enc.Close()
		return fmt.Errorf("gateway reverse sync: marshal yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("gateway reverse sync: marshal yaml: %w", err)
	}
	return PersistYamlText(buf.String())
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setScalar updates an existing scalar child in place (keeping its comments)
// or appends the key when missing. Non-scalar children are never clobbered.
func setScalar(mapping *yaml.Node, key, value string) {
	setScalarTagged(mapping, key, value, "!!str")
}

func setInt(mapping *yaml.Node, key string, value int) {
	setScalarTagged(mapping, key, strconv.Itoa(value), "!!int")
}

func setBool(mapping *yaml.Node, key string, value bool) {
	setScalarTagged(mapping, key, strconv.FormatBool(value), "!!bool")
}

func setScalarTagged(mapping *yaml.Node, key, value, tag string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			if v.Kind != yaml.ScalarNode {
				return
			}
			// Style is reset to plain; the encoder re-quotes automatically
			// when the value is not valid as a plain scalar.
			v.Tag, v.Value, v.Style = tag, value, 0
			return
		}
	}
	appendScalarTagged(mapping, key, value, tag)
}

func appendScalar(mapping *yaml.Node, key, value string) {
	appendScalarTagged(mapping, key, value, "!!str")
}

func appendInt(mapping *yaml.Node, key string, value int) {
	appendScalarTagged(mapping, key, strconv.Itoa(value), "!!int")
}

func appendBool(mapping *yaml.Node, key string, value bool) {
	appendScalarTagged(mapping, key, strconv.FormatBool(value), "!!bool")
}

func appendScalarTagged(mapping *yaml.Node, key, value, tag string) {
	appendNode(mapping, key, &yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: tag})
}

func appendNode(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
}

func strScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s, Tag: "!!str"}
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
