package webui_test

import (
	"crypto/sha256"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	// resourceTag matches every element that makes the browser load a
	// resource. An <a> element is navigation, not a load, so it may name the
	// pull request URL.
	resourceTag = regexp.MustCompile(`(?is)<(?:script|link|img|iframe|frame|source|object|embed|audio|video|track|input|base)\b[^>]*>`)
	// resourceURL matches the URL attributes of one resource element.
	resourceURL = regexp.MustCompile(`(?is)\s(?:src|href|data|srcset|poster)\s*=\s*["']?([^"'\s>]*)`)
	// formAction matches every form target anywhere in a page.
	formAction = regexp.MustCompile(`(?is)\s(?:action|formaction)\s*=\s*["']?([^"'\s>]*)`)
	// cssURL matches every url() reference in inline or served CSS.
	cssURL = regexp.MustCompile(`(?is)url\(\s*["']?([^"')\s]*)`)
	// absoluteURL matches a scheme URL or a protocol-relative string literal
	// in a served asset.
	absoluteURL = regexp.MustCompile(`(?i)https?://|["'\x60]//`)
	// listedAsset matches one entry of the asset directory listing.
	listedAsset = regexp.MustCompile(`href="([^"]+)"`)
)

// assertNoExternalResources fails when a rendered page makes the browser load
// anything from another origin: every resource element, form target, and CSS
// url() must be a same-origin path, and no CSS may @import.
func assertNoExternalResources(t *testing.T, body string) {
	t.Helper()

	for _, tag := range resourceTag.FindAllString(body, -1) {
		for _, match := range resourceURL.FindAllStringSubmatch(tag, -1) {
			assertSameOriginPath(t, match[1], tag)
		}
	}
	for _, match := range formAction.FindAllStringSubmatch(body, -1) {
		assertSameOriginPath(t, match[1], match[0])
	}
	assertNoExternalCSS(t, body)
}

// assertNoExternalCSS fails when CSS imports a stylesheet or references a URL
// that is not a same-origin path.
func assertNoExternalCSS(t *testing.T, css string) {
	t.Helper()

	if strings.Contains(strings.ToLower(css), "@import") {
		t.Errorf("CSS uses @import")
	}
	for _, match := range cssURL.FindAllStringSubmatch(css, -1) {
		assertSameOriginPath(t, match[1], match[0])
	}
}

// assertSameOriginPath fails unless reference is an absolute path on the UI's
// own origin.
func assertSameOriginPath(t *testing.T, reference, context string) {
	t.Helper()

	if !strings.HasPrefix(reference, "/") || strings.HasPrefix(reference, "//") || strings.HasPrefix(reference, `/\`) {
		t.Errorf("%q loads %q, want a same-origin path", context, reference)
	}
}

// assertServedAssetsAreSelfContained lists the embedded assets through the
// handler and fails when one names an absolute or protocol-relative URL.
func assertServedAssetsAreSelfContained(t *testing.T, handler http.Handler) {
	t.Helper()

	listing := get(t, handler, "/assets/")
	if listing.Code != http.StatusOK {
		t.Fatalf("GET /assets/ status = %d, want 200", listing.Code)
	}
	assets := listedAsset.FindAllStringSubmatch(listing.Body.String(), -1)
	if len(assets) == 0 {
		t.Fatalf("GET /assets/ lists no asset")
	}
	for _, asset := range assets {
		response := get(t, handler, "/assets/"+asset[1])
		if response.Code != http.StatusOK {
			t.Fatalf("GET /assets/%s status = %d, want 200", asset[1], response.Code)
		}
		content := response.Body.String()
		if absoluteURL.MatchString(content) {
			t.Errorf("asset %s names an absolute URL", asset[1])
		}
		if strings.HasSuffix(asset[1], ".css") {
			assertNoExternalCSS(t, content)
		}
	}
}

// storeSnapshot is the observable state of the operational store directory.
type storeSnapshot struct {
	digest  [sha256.Size]byte
	modTime time.Time
	mode    os.FileMode
	entries string
}

// snapshotStore records the database bytes, modification time, and mode,
// and the sorted directory listing, so a journal or backup file also shows.
func snapshotStore(t *testing.T, storePath string) storeSnapshot {
	t.Helper()

	content, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(storePath))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return storeSnapshot{digest: sha256.Sum256(content), modTime: info.ModTime(), mode: info.Mode(), entries: strings.Join(names, "\n")}
}

// makeStoreReadOnly removes write permission from the store directory and
// database until the test ends, so any write attempt fails loudly.
func makeStoreReadOnly(t *testing.T, storePath string) {
	t.Helper()

	directory := filepath.Dir(storePath)
	if err := os.Chmod(storePath, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(directory, 0o700)
		_ = os.Chmod(storePath, 0o600)
	})
}
