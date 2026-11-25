package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string // prefix the output must start with; "" means empty
		wantStderr bool   // whether a diagnostic is expected on stderr
	}{
		{name: "version flag", args: []string{"-version"}, wantCode: exitOK, wantStdout: "tinycask " + version},
		{name: "double-dash version flag", args: []string{"--version"}, wantCode: exitOK, wantStdout: "tinycask " + version},
		{name: "help", args: []string{"-h"}, wantCode: exitOK, wantStderr: true},
		{name: "unknown flag", args: []string{"-bogus"}, wantCode: exitUsage, wantStderr: true},
		{name: "stray argument", args: []string{"serve"}, wantCode: exitUsage, wantStderr: true},
		{name: "no flags", args: nil, wantCode: exitUsage, wantStderr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer

			code := run(tt.args, &stdout, &stderr)

			assert.Equal(t, tt.wantCode, code)
			if tt.wantStdout == "" {
				assert.Empty(t, stdout.String())
			} else {
				assert.True(t, bytes.HasPrefix(stdout.Bytes(), []byte(tt.wantStdout)),
					"stdout %q does not start with %q", stdout.String(), tt.wantStdout)
			}
			assert.Equal(t, tt.wantStderr, stderr.Len() > 0, "stderr: %q", stderr.String())
		})
	}
}

// failWriter fails every write, to check that output errors are not ignored.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, assert.AnError }

func TestRun_VersionWriteError(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer

	code := run([]string{"-version"}, failWriter{}, &stderr)

	assert.Equal(t, exitUsage, code)
}
