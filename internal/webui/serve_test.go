package webui_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stevie1704/sw-factory/internal/webui"
)

// TestValidateAddressAcceptsOnlyLoopbackIPv4WithPort keeps the UI reachable
// only from this host: any other host, a wildcard, or a missing port is
// refused before a socket opens.
func TestValidateAddressAcceptsOnlyLoopbackIPv4WithPort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		valid   bool
	}{
		{address: "127.0.0.1:8765", valid: true},
		{address: "127.0.0.1:0", valid: true},
		{address: "127.0.0.1:65535", valid: true},
		{address: "", valid: false},
		{address: "127.0.0.1", valid: false},
		{address: "127.0.0.1:", valid: false},
		{address: ":8765", valid: false},
		{address: "0.0.0.0:8765", valid: false},
		{address: "[::]:8765", valid: false},
		{address: "[::1]:8765", valid: false},
		{address: "localhost:8765", valid: false},
		{address: "192.168.1.10:8765", valid: false},
		{address: "127.0.0.2:8765", valid: false},
		{address: "127.0.0.1:http", valid: false},
		{address: "127.0.0.1:-1", valid: false},
		{address: "127.0.0.1:65536", valid: false},
	}
	for _, test := range tests {
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()

			err := webui.ValidateAddress(test.address)
			if test.valid && err != nil {
				t.Fatalf("ValidateAddress(%q) = %v, want nil", test.address, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("ValidateAddress(%q) = nil, want an error", test.address)
			}
		})
	}
}

// TestServeAnswersOnTheBoundURLUntilCancelled verifies Serve reports the URL
// it bound, answers requests there, and returns nil after a graceful stop.
func TestServeAnswersOnTheBoundURLUntilCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "factory ui")
	})
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- webui.Serve(ctx, "127.0.0.1:0", handler, func(url string) { urls <- url })
	}()

	var url string
	select {
	case url = <-urls:
	case err := <-done:
		t.Fatalf("Serve returned before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not report a URL")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") || strings.HasSuffix(url, ":0") {
		t.Fatalf("ready URL = %q, want http://127.0.0.1:<bound port>", url)
	}
	response, err := http.Get(url + "/")
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "factory ui" {
		t.Fatalf("GET %s body = %q, %v; want factory ui", url, body, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after cancel")
	}
}

// TestServeRefusesANonLoopbackAddress verifies Serve itself never opens a
// socket on another interface, even when a caller skips ValidateAddress.
func TestServeRefusesANonLoopbackAddress(t *testing.T) {
	t.Parallel()

	ready := func(string) { t.Error("Serve reported ready for 0.0.0.0") }
	if err := webui.Serve(t.Context(), "0.0.0.0:0", http.NotFoundHandler(), ready); err == nil {
		t.Fatal("Serve(0.0.0.0:0) = nil, want an error")
	}
}
