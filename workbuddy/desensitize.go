package main

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	zeroWidthSpace           = "​"
	oauthClientModeCLI       = "cli"
	oauthClientModeWorkBuddy = "workbuddy"
)

var defaultDesensitizeTerms = []string{
	"DoS", "DDoS", "exploit", "credential testing", "credential stuffing",
	"supply chain compromise", "supply-chain compromise", "detection evasion",
	"C2 frameworks", "C2 framework", "command and control", "malicious purposes",
	"malicious intent", "mass targeting", "brute force", "brute-force",
	"privilege escalation", "reverse shell", "remote code execution", "SQL injection",
	"XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
	"backdoor", "botnet", "zero-day", "0day", "vulnerability", "vulnerabilities",
	"red teaming", "red-teaming", "sandbox", "sandboxing", "sandboxed", "unsandboxed",
	"escalated privileges", "escalated", "escalation", "destructive action",
	"destructive command", "destructive", "attack", "attacks", "cybersecurity",
	"security review", "exploit development", "hacking", "penetration testing",
	"penetration test", "injection", "weaponize", "weaponized", "harmful", "dangerous",
	"abuse", "abusive", "illegal", "terrorist", "terrorism", "bomb", "weapon",
	"weapons", "drug", "drugs", "narcotic", "suicide", "self-harm", "murder",
	"kill", "violence", "violent", "Claude Code", "Claude Opus", "Claude Sonnet",
	"Claude Haiku", "Claude Fable", "Anthropic", "Co-Authored-By",
	"noreply@anthropic.com", "Codex", "codex",
}

type desensitizeMatcher struct {
	expression *regexp.Regexp
}

type featureRuntimeConfig struct {
	desensitizeEnabled bool
	desensitizeTerms   []string
	desensitizeSource  string
	matcher            *desensitizeMatcher
	oauthClientMode    string
	enterpriseCredits  bool
}

var featureRuntime atomic.Pointer[featureRuntimeConfig]

func init() {
	cfg, err := parseFeatureRuntime(nil)
	if err != nil {
		panic(err)
	}
	featureRuntime.Store(cfg)
}

func currentFeatureRuntime() *featureRuntimeConfig {
	cfg := featureRuntime.Load()
	if cfg == nil {
		return nil
	}
	snapshot := *cfg
	snapshot.desensitizeTerms = append([]string(nil), cfg.desensitizeTerms...)
	return &snapshot
}

type featureConfigYAML struct {
	Desensitize       *bool     `yaml:"desensitize"`
	DesensitizeTerms  *[]string `yaml:"desensitize_terms"`
	OAuthClientMode   string    `yaml:"oauth_client_mode"`
	EnterpriseCredits *bool     `yaml:"enterprise_credits"`
}

func parseFeatureRuntime(raw []byte) (*featureRuntimeConfig, error) {
	var doc featureConfigYAML
	if strings.TrimSpace(string(raw)) != "" {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, errors.New("invalid config_yaml")
		}
	}

	mode := strings.ToLower(strings.TrimSpace(doc.OAuthClientMode))
	if mode == "" {
		mode = oauthClientModeCLI
	}
	if mode != oauthClientModeCLI && mode != oauthClientModeWorkBuddy {
		return nil, errors.New("oauth_client_mode must be cli or workbuddy")
	}

	terms, source, err := normalizedDesensitizeTerms(doc.DesensitizeTerms)
	if err != nil {
		return nil, err
	}
	matcher, err := compileDesensitizeMatcher(terms)
	if err != nil {
		return nil, err
	}
	return &featureRuntimeConfig{
		desensitizeEnabled: doc.Desensitize != nil && *doc.Desensitize,
		desensitizeTerms:   terms,
		desensitizeSource:  source,
		matcher:            matcher,
		oauthClientMode:    mode,
		enterpriseCredits:  doc.EnterpriseCredits != nil && *doc.EnterpriseCredits,
	}, nil
}

func normalizedDesensitizeTerms(configured *[]string) ([]string, string, error) {
	source := "custom"
	input := []string(nil)
	if configured == nil {
		source = "default"
		input = defaultDesensitizeTerms
	} else {
		input = *configured
	}

	terms := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, raw := range input {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		if utf8.RuneCountInString(term) < 2 {
			return nil, "", errors.New("desensitize_terms entries must contain at least two Unicode runes")
		}
		if strings.Contains(term, zeroWidthSpace) {
			return nil, "", errors.New("desensitize_terms entries must not contain U+200B")
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms, source, nil
}

func compileDesensitizeMatcher(terms []string) (*desensitizeMatcher, error) {
	ordered := append([]string(nil), terms...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return utf8.RuneCountInString(ordered[i]) > utf8.RuneCountInString(ordered[j])
	})

	alternatives := make([]string, 0, len(ordered))
	for _, term := range ordered {
		duplicate := false
		for _, existing := range alternatives {
			if strings.EqualFold(term, existing) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			alternatives = append(alternatives, term)
		}
	}
	if len(alternatives) == 0 {
		return &desensitizeMatcher{}, nil
	}
	quoted := make([]string, len(alternatives))
	for i, term := range alternatives {
		quoted[i] = regexp.QuoteMeta(term)
	}
	expression, err := regexp.Compile("(?i:" + strings.Join(quoted, "|") + ")")
	if err != nil {
		return nil, err
	}
	return &desensitizeMatcher{expression: expression}, nil
}

func (m *desensitizeMatcher) replace(input string) string {
	if m == nil || m.expression == nil || input == "" {
		return input
	}
	for {
		changed := false
		next := m.expression.ReplaceAllStringFunc(input, func(match string) string {
			_, size := utf8.DecodeRuneInString(match)
			changed = true
			return match[:size] + zeroWidthSpace + match[size:]
		})
		if !changed {
			return input
		}
		input = next
	}
}
