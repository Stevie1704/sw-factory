package cli_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/cli"
)

// TestRunUIRefusesInvalidArguments verifies the UI verb refuses a
// non-loopback address, a negative refresh interval, or a positional argument
// as a usage error before it opens a socket.
func TestRunUIRefusesInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "wildcard address", args: []string{"--address", "0.0.0.0:0"}, want: "the host must be 127.0.0.1"},
		{name: "localhost address", args: []string{"--address", "localhost:8765"}, want: "the host must be 127.0.0.1"},
		{name: "IPv6 loopback address", args: []string{"--address", "[::1]:8765"}, want: "the host must be 127.0.0.1"},
		{name: "address without port", args: []string{"--address", "127.0.0.1"}, want: "invalid UI address"},
		{name: "negative refresh", args: []string{"--refresh", "-5s"}, want: "--refresh must not be negative"},
		{name: "positional argument", args: []string{"extra-arg"}, want: "does not accept positional arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			args := append([]string{"ui", "--config", configPath}, test.args...)
			var output bytes.Buffer
			code := cli.Run(context.Background(), args, &output, &output)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2, output = %s", code, output.String())
			}
			if !strings.Contains(output.String(), test.want) {
				t.Fatalf("output = %q, want %q", output.String(), test.want)
			}
			if strings.Contains(output.String(), "listening") {
				t.Fatalf("UI started for refused arguments: %q", output.String())
			}
		})
	}
}

// TestRunUIServesUntilCancelled verifies the UI verb reports its loopback
// URL, answers there, and exits 0 with a stop line when its context ends.
func TestRunUIServesUntilCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	output := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"ui", "--config", configPath, "--address", "127.0.0.1:0", "--refresh", "2s"}, output, output)
	}()

	url := waitForUIURL(t, output, done)
	response, err := http.Get(url + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "Operational store unavailable") {
		t.Fatalf("GET / = %d %q, want the store-unavailable page", response.StatusCode, body)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0, output = %s", code, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ui did not stop after cancel")
	}
	if !strings.Contains(output.String(), "factory ui stopped") {
		t.Fatalf("output = %q, want a stop line", output.String())
	}
}

// TestRunUIZeroRefreshTurnsAutomaticRefreshOff verifies --refresh 0 starts
// the UI and serves pages that do not load the refresh script.
func TestRunUIZeroRefreshTurnsAutomaticRefreshOff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	output := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- cli.Run(ctx, []string{"ui", "--config", configPath, "--address", "127.0.0.1:0", "--refresh", "0"}, output, output)
	}()

	url := waitForUIURL(t, output, done)
	response, err := http.Get(url + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "refresh.js") {
		t.Fatalf("page with --refresh 0 loads the refresh script: %q", body)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0, output = %s", code, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ui did not stop after cancel")
	}
}

// uiListeningLine matches the line the UI verb prints once it is bound.
var uiListeningLine = regexp.MustCompile(`factory ui listening on (http://127\.0\.0\.1:[1-9][0-9]*)`)

// waitForUIURL returns the URL from the listening line, or fails when the
// verb exits or does not bind in time.
func waitForUIURL(t *testing.T, output *lockedBuffer, done <-chan int) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if match := uiListeningLine.FindStringSubmatch(output.String()); match != nil {
			return match[1]
		}
		select {
		case code := <-done:
			t.Fatalf("ui exited with %d before listening, output = %s", code, output.String())
		case <-deadline:
			t.Fatalf("ui did not report a URL, output = %s", output.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// lockedBuffer is an output writer that a running command and the test can
// use at the same time.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// Write appends p under the lock.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

// String returns a copy of everything written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
