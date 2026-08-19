package main

import (
	"strings"
	"testing"
)

func TestRegistrationConfigFieldsMatchImplementedConfig(t *testing.T) {
	fields := wbRegistration().Metadata.ConfigFields
	managementKeyCount := 0
	for _, field := range fields {
		switch field.Name {
		case "management_key":
			managementKeyCount++
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
}
