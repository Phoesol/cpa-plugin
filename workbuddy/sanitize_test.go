package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeBlockedTemplates_ClaudeCode(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude."
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace blocked template")
	}
	want := "You are Claude Code, Anthropic's official CLI tool for Claude."
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestSanitizeBlockedTemplates_MainBranch(t *testing.T) {
	in := "Main branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if out == in {
		t.Fatal("should replace Main branch")
	}
}

func TestSanitizeBlockedTemplates_NoMatch(t *testing.T) {
	in := "Hello world"
	out := sanitizeBlockedTemplates(in)
	if out != in {
		t.Fatal("should pass through unchanged")
	}
}

func TestSanitizeBlockedTemplates_Fingerprints(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "partial template text remains unchanged",
			in:   "    You are Claude Code, but this is not the blocked template",
			want: "    You are Claude Code, but this is not the blocked template",
		},
		{
			name: "ordinary cc assignment",
			in:   "Set cc_library=foo and keep this sentence",
			want: "Set cc_library=foo and keep this sentence",
		},
		{
			name: "billing header value irrelevant",
			in:   "prefix x-anthropic-billing-header: arbitrary=value; suffix",
			want: "prefix suffix",
		},
		{
			name: "billing header case insensitive",
			in:   "X-Anthropic-Billing-Header: cc_version=1.0; useful",
			want: "useful",
		},
		{
			name: "cc fingerprint case insensitive",
			in:   "semantic; CC_VERSION=2.0; CC_ENTRYPOINT=cli; end",
			want: "semantic; end",
		},
		{
			name: "multiple trailing cc keys",
			in:   "semantic; cc_version=2.0; cc_entrypoint=cli; end",
			want: "semantic; end",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBlockedTemplates(tt.in); got != tt.want {
				t.Fatalf("sanitizeBlockedTemplates(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPrepareUpstreamBodySanitizesFingerprintsAndPreservesReasoningEffort(t *testing.T) {
	body := []byte(`{"model":"other-model","reasoning_effort":"low","messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; keep me"}]}`)
	out := prepareUpstreamBody(body, nil, nil, "other-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", obj["reasoning_effort"])
	}
	messages := obj["messages"].([]any)
	content := messages[0].(map[string]any)["content"].(string)
	if strings.Contains(strings.ToLower(content), "x-anthropic-billing-header") || strings.Contains(strings.ToLower(content), "cc_") {
		t.Fatalf("fingerprints remain: %q", content)
	}
	if !strings.Contains(content, "keep me") {
		t.Fatalf("semantic text lost: %q", content)
	}
}

func TestPrepareUpstreamBodyAppliesThinkingPolicyToResolvedModel(t *testing.T) {
	tests := []struct {
		name          string
		clientModel   string
		upstreamModel string
		effort        string
		wantEffort    string
	}{
		{
			name:          "alias to hy3 x",
			clientModel:   "fast",
			upstreamModel: "hy3-x",
			effort:        "low",
			wantEffort:    "high",
		},
		{
			name:          "hy3 prefixed alias to hy4 preview",
			clientModel:   "hy3-fast",
			upstreamModel: "hy4-preview",
			effort:        "low",
			wantEffort:    "high",
		},
		{
			name:          "alias to hy4 preview x",
			clientModel:   "preview-fast",
			upstreamModel: "hy4-preview-x",
			effort:        "medium",
			wantEffort:    "high",
		},
		{
			name:          "hy3 prefixed alias to glm 5.3",
			clientModel:   "hy3-glm",
			upstreamModel: "glm-5.3",
			effort:        "low",
			wantEffort:    "xhigh",
		},
		{
			name:          "glm alias to glm 5.3 flash",
			clientModel:   "glm-5.3-fast",
			upstreamModel: "glm-5.3-flash",
			effort:        "low",
			wantEffort:    "max",
		},
		{
			name:          "forced-looking alias to unknown model",
			clientModel:   "glm-5.3",
			upstreamModel: "other-model",
			effort:        "medium",
			wantEffort:    "medium",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamModel := resolveUpstreamModel(tt.clientModel, map[string]string{
				"oauth-model-alias": tt.clientModel + "=" + tt.upstreamModel,
			})
			if upstreamModel != tt.upstreamModel {
				t.Fatalf("resolved model = %q, want %q", upstreamModel, tt.upstreamModel)
			}
			body := []byte(`{"model":"` + tt.clientModel + `","reasoning_effort":"` + tt.effort + `","messages":[]}`)
			out := prepareUpstreamBody(body, nil, nil, upstreamModel)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatal(err)
			}
			if got := obj["model"]; got != tt.upstreamModel {
				t.Fatalf("model = %v, want %q", got, tt.upstreamModel)
			}
			if got := obj["reasoning_effort"]; got != tt.wantEffort {
				t.Fatalf("reasoning_effort = %v, want %q", got, tt.wantEffort)
			}
		})
	}
}

func TestPrepareUpstreamBodyAppliesThinkingPolicyAfterModelWhitespaceNormalization(t *testing.T) {
	requestedModel := "glm-5.3 "
	upstreamModel := resolveUpstreamModel(requestedModel, nil)
	if upstreamModel != "glm-5.3" {
		t.Fatalf("resolved model = %q, want %q", upstreamModel, "glm-5.3")
	}

	body := []byte(`{"model":"glm-5.3 ","reasoning_effort":"low","messages":[]}`)
	out := prepareUpstreamBody(body, nil, nil, upstreamModel)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if got := obj["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want %q", got, "xhigh")
	}
	if got := obj["model"]; got != "glm-5.3" {
		t.Fatalf("model = %v, want %q", got, "glm-5.3")
	}
}

func TestForceMaxThinking_InsertsMissingFixedModelEfforts(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{model: "hy3", want: "high"},
		{model: "glm-5.3", want: "xhigh"},
		{model: "glm-5.3-flash", want: "max"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			obj := map[string]any{"model": tt.model}
			if !forceMaxThinking(obj) {
				t.Fatal("expected thinking effort insertion")
			}
			if got := obj["reasoning_effort"]; got != tt.want {
				t.Fatalf("reasoning_effort = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestForceMaxThinking_FixedModelEfforts(t *testing.T) {
	tests := []struct {
		model string
		input string
		want  string
	}{
		{model: "hy3", input: "low", want: "high"},
		{model: "hy3-x", input: "max", want: "high"},
		{model: "hy3-preview-agent", input: "low", want: "high"},
		{model: "hy4-preview", input: "low", want: "high"},
		{model: "hy4-preview-x", input: "max", want: "high"},
		{model: "glm-5.3", input: "low", want: "xhigh"},
		{model: "glm-5.3-flash", input: "low", want: "max"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			obj := map[string]any{
				"model":            tt.model,
				"reasoning_effort": tt.input,
			}
			if !forceMaxThinking(obj) {
				t.Fatal("expected thinking effort change")
			}
			if got := obj["reasoning_effort"]; got != tt.want {
				t.Fatalf("reasoning_effort = %v, want %q", got, tt.want)
			}
		})
	}
}

func TestForceMaxThinking_NonHy3Model(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change non-hy3 model")
	}
}

func TestForceMaxThinking_AlreadyHigh(t *testing.T) {
	obj := map[string]any{"model": "hy3-std", "reasoning_effort": "high"}
	changed := forceMaxThinking(obj)
	if changed {
		t.Fatal("should not change when already high")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}
