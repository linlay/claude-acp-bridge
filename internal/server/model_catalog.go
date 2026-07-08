package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"claude-acp-bridge/internal/platform"
)

var claudeReasoningEfforts = []string{"LOW", "MEDIUM", "HIGH", "XHIGH", "MAX"}

type claudeRuntimeModelInfo struct {
	ModelKey      string
	ContextWindow int
}

type claudeModelDiscoveryLine struct {
	Model      string                              `json:"model"`
	Message    *claudeModelDiscoveryMessage        `json:"message"`
	Event      *claudeModelDiscoveryStreamEvent    `json:"event"`
	ModelUsage map[string]claudeModelDiscoveryMeta `json:"modelUsage"`
}

type claudeModelDiscoveryStreamEvent struct {
	Message *claudeModelDiscoveryMessage `json:"message"`
}

type claudeModelDiscoveryMessage struct {
	Model string `json:"model"`
}

type claudeModelDiscoveryMeta struct {
	ContextWindow int `json:"contextWindow"`
}

func parseClaudeModelCatalog(output []byte) (platform.ModelCatalogResponse, error) {
	info, err := parseClaudeRuntimeModelInfo(output)
	if err != nil {
		return platform.ModelCatalogResponse{}, err
	}
	return buildClaudeModelCatalog(info), nil
}

func parseClaudeRuntimeModelInfo(output []byte) (claudeRuntimeModelInfo, error) {
	info := claudeRuntimeModelInfo{}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var event claudeModelDiscoveryLine
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		info.setModel(event.Model)
		if event.Message != nil {
			info.setModel(event.Message.Model)
		}
		if event.Event != nil && event.Event.Message != nil {
			info.setModel(event.Event.Message.Model)
		}
		keys := make([]string, 0, len(event.ModelUsage))
		for key := range event.ModelUsage {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			info.setModel(key)
			if usage := event.ModelUsage[key]; usage.ContextWindow > 0 && info.ContextWindow == 0 {
				info.ContextWindow = usage.ContextWindow
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return claudeRuntimeModelInfo{}, err
	}
	if strings.TrimSpace(info.ModelKey) == "" {
		return claudeRuntimeModelInfo{}, errors.New("claude model discovery output did not contain model data")
	}
	return info, nil
}

func (i *claudeRuntimeModelInfo) setModel(model string) {
	if model = strings.TrimSpace(model); model != "" && strings.TrimSpace(i.ModelKey) == "" {
		i.ModelKey = model
	}
}

func buildClaudeModelCatalog(info claudeRuntimeModelInfo) platform.ModelCatalogResponse {
	modelKey := strings.TrimSpace(info.ModelKey)
	if modelKey == "" {
		return platform.ModelCatalogResponse{}
	}
	return platform.ModelCatalogResponse{Models: []platform.ModelCatalogItem{{
		Key:              modelKey,
		Name:             modelKey,
		ModelID:          modelKey,
		ContextWindow:    info.ContextWindow,
		IsReasoner:       true,
		ReasoningEfforts: append([]string(nil), claudeReasoningEfforts...),
	}}}
}
