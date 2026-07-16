package main

import (
	"errors"
	"testing"
)

func TestParseAutostartState(t *testing.T) {
	tests := []struct {
		name string
		run  func() (bool, error)
		want bool
	}{
		{
			name: "windows enabled by default",
			run: func() (bool, error) {
				return parseWindowsTaskEnabled([]byte(`<Task><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy></Settings></Task>`))
			},
			want: true,
		},
		{
			name: "windows disabled",
			run: func() (bool, error) {
				return parseWindowsTaskEnabled([]byte(`<Task><Settings><Enabled>false</Enabled></Settings></Task>`))
			},
		},
		{
			name: "linux enabled",
			run: func() (bool, error) {
				return parseLinuxAutostartState([]byte("enabled\n"), nil)
			},
			want: true,
		},
		{
			name: "linux disabled",
			run: func() (bool, error) {
				return parseLinuxAutostartState([]byte("disabled\n"), errors.New("exit status 1"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.run()
			if err != nil || got != test.want {
				t.Fatalf("enabled=%v err=%v, want %v", got, err, test.want)
			}
		})
	}

	if !macOSAutostartDisabled([]byte(`disabled services = { "com.codex-quota-router" => true }`)) {
		t.Fatal("macOS disabled entry was not detected")
	}
	if macOSAutostartDisabled([]byte(`disabled services = { "com.codex-quota-router" => false }`)) {
		t.Fatal("macOS enabled entry was reported as disabled")
	}
}
