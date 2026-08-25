package main

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRegistrationConfigFieldsMatchImplementedConfig(t *testing.T) {
	fields := wbRegistration().Metadata.ConfigFields
	managementKeyCount := 0
	proxyURLCount := 0
	proxyDescription := ""
	for _, field := range fields {
		switch field.Name {
		case "management_key":
			managementKeyCount++
		case "proxy-url":
			proxyURLCount++
			if field.Type != pluginapi.ConfigFieldTypeString {
				t.Fatalf("proxy-url type = %q, want string", field.Type)
			}
			proxyDescription = strings.ToLower(field.Description)
		case "proxy_url":
			t.Fatal("proxy_url alias must not be registered")
		case "scheduler_mode":
			if strings.Contains(strings.ToLower(field.Description), "highest remaining") {
				t.Fatal("scheduler_mode description advertises unimplemented highest-credit ranking")
			}
			if !strings.Contains(strings.ToLower(field.Description), "panel-selected") {
				t.Fatal("scheduler_mode description must document panel-selected routing")
			}
		}
	}
	if managementKeyCount != 1 {
		t.Fatalf("management_key config field count = %d", managementKeyCount)
	}
	if proxyURLCount != 1 {
		t.Fatalf("proxy-url config field count = %d", proxyURLCount)
	}
	for _, required := range []string{"http", "socks5", "socks5h", "inherit", "fail closed", "request-log"} {
		if !strings.Contains(proxyDescription, required) {
			t.Errorf("proxy-url description missing %q: %q", required, proxyDescription)
		}
	}
}
