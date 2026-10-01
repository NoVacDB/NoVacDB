package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    config
		wantErr error
	}{
		{"defaults", nil, config{dataDir: defaultDataDir, port: defaultPort}, nil},
		{"custom values", []string{"--data-dir", "/x", "--port", "6000"}, config{dataDir: "/x", port: 6000}, nil},
		{"minimum port", []string{"--port", "1"}, config{dataDir: defaultDataDir, port: 1}, nil},
		{"maximum port", []string{"--port", "65535"}, config{dataDir: defaultDataDir, port: 65535}, nil},
		{"port zero", []string{"--port", "0"}, config{}, ErrInvalidPort},
		{"port too large", []string{"--port", "65536"}, config{}, ErrInvalidPort},
		{"negative port", []string{"--port", "-1"}, config{}, ErrInvalidPort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFlags(tc.args, &bytes.Buffer{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("config = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseFlagsRejectsGarbage(t *testing.T) {
	for _, args := range [][]string{{"--port", "abc"}, {"--nope"}} {
		if _, err := parseFlags(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseFlags(%v) succeeded, want error", args)
		}
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"clean exit", nil, 0, "not implemented yet"},
		{"help", []string{"-h"}, 0, "data-dir"},
		{"bad port", []string{"--port", "0"}, 2, "port must be"},
		{"bad flag", []string{"--nope"}, 2, "error:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := run(tc.args, &out); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (output: %s)", code, tc.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tc.wantOut)
			}
		})
	}
}
