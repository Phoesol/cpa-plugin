// body.go constructs the QoderWork agent_chat_generation request body from
// OpenAI-style chat completion inputs.
//
// The base template lives in baseprompt.json (embedded). Per-request we
// overwrite request/session ids, timestamps, model key, and the user prompt.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

//go:embed baseprompt.json
var basepromptJSON []byte

// qwModel describes one entry from the upstream model list.
type qwModel struct {
	Key             string  `json:"key"`
	DisplayName     string  `json:"display_name"`
	Format          string  `json:"format"`
	Source          string  `json:"source"`
	Enable          bool    `json:"enable"`
	IsDefault       bool    `json:"is_default"`
	IsVL            bool    `json:"is_vl"`
	IsReasoning     bool    `json:"is_reasoning"`
	PriceFactor     float64 `json:"price_factor"`
	MaxInputTokens  int     `json:"max_input_tokens"`
}

// staticModels is the fallback list, mirrored from /root/qoderwork/models_list.json
// (fetched 2026-07-26). Dynamic fetching can refresh this at runtime.
var staticModels = []qwModel{
	{Key: "auto", DisplayName: "Auto", Format: "openai", Source: "system", Enable: true, IsDefault: true, IsVL: true, IsReasoning: true, PriceFactor: 0.5, MaxInputTokens: 180000},
	{Key: "qmodel_preview", DisplayName: "Qwen3.8-Max-Preview", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.05, MaxInputTokens: 180000},
	{Key: "qmodel_latest", DisplayName: "Qwen3.7-Max", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.25, MaxInputTokens: 180000},
	{Key: "qmodel", DisplayName: "Qwen3.7-Plus", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.1, MaxInputTokens: 180000},
	{Key: "q36fmodel", DisplayName: "Qwen3.6-Flash", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.1, MaxInputTokens: 180000},
	{Key: "dmodel", DisplayName: "DeepSeek-V4-Pro", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.5, MaxInputTokens: 180000},
	{Key: "dfmodel", DisplayName: "DeepSeek-V4-Flash", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: false, PriceFactor: 0.1, MaxInputTokens: 180000},
	{Key: "gm51model", DisplayName: "GLM-5.2", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.6, MaxInputTokens: 180000},
	{Key: "kmodel", DisplayName: "Kimi-K2.7-Code", Format: "openai", Source: "system", Enable: true, IsVL: true, IsReasoning: true, PriceFactor: 0.3, MaxInputTokens: 180000},
	{Key: "mmodel", DisplayName: "MiniMax-M2.7", Format: "openai", Source: "system", Enable: true, IsVL: false, IsReasoning: false, PriceFactor: 0.2, MaxInputTokens: 180000},
}

// cpaToUpstreamKey maps CPA-facing model names to upstream keys.
// Unknown names pass through unchanged (server silently routes to auto).
func cpaToUpstreamKey(cpaModel string) string {
	switch cpaModel {
	case "qoder-auto", "auto":
		return "auto"
	case "qwen3.8-max-preview", "qwen3.8-max", "qmodel_preview":
		return "qmodel_preview"
	case "qwen3.7-max", "qmodel_latest":
		return "qmodel_latest"
	case "qwen3.7-plus", "qmodel":
		return "qmodel"
	case "qwen3.6-flash", "q36fmodel":
		return "q36fmodel"
	case "deepseek-v4-pro", "dmodel":
		return "dmodel"
	case "deepseek-v4-flash", "dfmodel":
		return "dfmodel"
	case "glm-5.2", "gm51model":
		return "gm51model"
	case "kimi-k2.7-code", "kmodel":
		return "kmodel"
	case "minimax-m2.7", "mmodel":
		return "mmodel"
	}
	return cpaModel
}

// openAIMessage is one message in the OpenAI chat completion format.
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openAIRequest is the CPA-facing chat completion request.
type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// extractLatestUserPrompt returns the content of the last user message.
func extractLatestUserPrompt(messages []openAIMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// buildQoderBody renders the upstream agent_chat_generation body for one request.
// modelKey is the upstream key (already mapped via cpaToUpstreamKey).
func buildQoderBody(req *openAIRequest, modelKey, userType string) ([]byte, error) {
	var base map[string]any
	if err := json.Unmarshal(basepromptJSON, &base); err != nil {
		return nil, fmt.Errorf("baseprompt decode: %w", err)
	}

	prompt := extractLatestUserPrompt(req.Messages)
	if prompt == "" {
		return nil, fmt.Errorf("no user message in request")
	}

	nid := uuid.NewString()
	base["request_id"] = nid
	base["chat_record_id"] = nid
	base["request_set_id"] = uuid.NewString()
	base["session_id"] = uuid.NewString()
	base["stream"] = true
	base["aliyun_user_type"] = userType
	base["agent_id"] = "agent_common"

	// model_config
	if mc, ok := base["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
	}

	// chat_context.text.text + chat_context.extra.originalContent.text
	if cc, ok := base["chat_context"].(map[string]any); ok {
		if txt, ok := cc["text"].(map[string]any); ok {
			txt["text"] = prompt
		}
		if extra, ok := cc["extra"].(map[string]any); ok {
			if oc, ok := extra["originalContent"].(map[string]any); ok {
				oc["text"] = prompt
			}
			if mc, ok := extra["modelConfig"].(map[string]any); ok {
				mc["key"] = modelKey
			}
		}
	}

	// messages: keep system prompt from baseprompt (template has it), replace user/assistant
	var systemMsgs []any
	if msgs, ok := base["messages"].([]any); ok {
		for _, m := range msgs {
			if mm, ok := m.(map[string]any); ok {
				if role, _ := mm["role"].(string); role == "system" {
					systemMsgs = append(systemMsgs, m)
				}
			}
		}
	}
	// Append the actual conversation
	for _, m := range req.Messages {
		systemMsgs = append(systemMsgs, map[string]any{
			"role":    m.Role,
			"content": m.Content,
		})
	}
	base["messages"] = systemMsgs

	// business
	if biz, ok := base["business"].(map[string]any); ok {
		biz["id"] = uuid.NewString()
		biz["begin_at"] = time.Now().UnixMilli()
		if len(prompt) > 30 {
			biz["name"] = prompt[:30]
		} else {
			biz["name"] = prompt
		}
	}

	return json.Marshal(base)
}
