package main

import "testing"

func TestUseDirectHTTPBridgeForWindows(t *testing.T) {
	tests := []struct {
		goos      string
		available bool
		want      bool
	}{
		{goos: "windows", available: true, want: true},
		{goos: "linux", available: true, want: false},
		{goos: "linux", available: false, want: true},
	}
	for _, tt := range tests {
		if got := useDirectHTTPBridgeFor(tt.goos, tt.available); got != tt.want {
			t.Fatalf("goos=%s available=%v: got %v, want %v", tt.goos, tt.available, got, tt.want)
		}
	}
}
