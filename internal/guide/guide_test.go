package guide_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Stevie1704/sw-factory/internal/guide"
)

// requiredTopics are the topics issue #239 requires every release to ship.
var requiredTopics = []string{"debug", "reporting", "setup"}

// TestTopicsShipEveryRequiredTopic verifies the topic index and that every
// listed topic has a body.
func TestTopicsShipEveryRequiredTopic(t *testing.T) {
	topics := guide.Topics()
	if strings.Join(topics, ",") != strings.Join(requiredTopics, ",") {
		t.Fatalf("Topics() = %v, want %v", topics, requiredTopics)
	}
	for _, name := range append([]string{guide.Overview}, topics...) {
		body, err := guide.Topic(name)
		if err != nil || strings.TrimSpace(body) == "" {
			t.Fatalf("Topic(%q) = %q, %v; want a body", name, body, err)
		}
	}
}

// TestOverviewStaysWithinWordCeiling enforces progressive disclosure.
func TestOverviewStaysWithinWordCeiling(t *testing.T) {
	body, err := guide.Topic(guide.Overview)
	if err != nil {
		t.Fatal(err)
	}
	if words := len(strings.Fields(body)); words > guide.OverviewWordLimit {
		t.Fatalf("overview has %d words, want at most %d", words, guide.OverviewWordLimit)
	}
}

// TestOverviewIndexesEveryTopic verifies the overview names every topic as a
// runnable command.
func TestOverviewIndexesEveryTopic(t *testing.T) {
	body, err := guide.Topic(guide.Overview)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range guide.Topics() {
		if !strings.Contains(body, "factory guide "+name) {
			t.Fatalf("overview does not index topic %q", name)
		}
	}
}

// TestTopicReferencesResolveThroughShippedResources verifies that a topic
// refers only to shipped topics and never to a checkout file, a relative
// link, or a build-machine path.
func TestTopicReferencesResolveThroughShippedResources(t *testing.T) {
	topicReference := regexp.MustCompile("factory guide ([a-z-]+)")
	relativeLink := regexp.MustCompile(`\]\((?:[^)#h]|h[^t])[^)]*\)`)
	hostPath := regexp.MustCompile(`/(?:Users|home)/[A-Za-z]`)
	known := map[string]bool{}
	for _, name := range guide.Topics() {
		known[name] = true
	}
	for _, name := range append([]string{guide.Overview}, guide.Topics()...) {
		body, err := guide.Topic(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range topicReference.FindAllStringSubmatch(body, -1) {
			if !known[match[1]] {
				t.Errorf("topic %q refers to unknown topic %q", name, match[1])
			}
		}
		if link := relativeLink.FindString(body); link != "" {
			t.Errorf("topic %q links to an unshipped resource %s", name, link)
		}
		if path := hostPath.FindString(body); path != "" {
			t.Errorf("topic %q contains a build-machine path %q", name, path)
		}
	}
}

// TestTopicRejectsUnknownName verifies that an unknown name is a typed error.
func TestTopicRejectsUnknownName(t *testing.T) {
	if _, err := guide.Topic("nonexistent"); !errors.Is(err, guide.ErrUnknownTopic) {
		t.Fatalf("Topic(nonexistent) error = %v, want ErrUnknownTopic", err)
	}
}

// TestPublishedDocsDoNotCopyGuideProse verifies that the embedded topics stay
// the only copy of their prose: README and docs link to them rather than
// restating their paragraphs.
func TestPublishedDocsDoNotCopyGuideProse(t *testing.T) {
	published := []string{filepath.Join("..", "..", "README.md")}
	documents, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	published = append(published, documents...)
	var corpus strings.Builder
	for _, path := range published {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		corpus.WriteString(strings.Join(strings.Fields(string(data)), " "))
	}
	for _, name := range append([]string{guide.Overview}, guide.Topics()...) {
		body, err := guide.Topic(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, paragraph := range strings.Split(body, "\n\n") {
			normalized := strings.Join(strings.Fields(paragraph), " ")
			if len(strings.Fields(normalized)) >= 12 && strings.Contains(corpus.String(), normalized) {
				t.Errorf("published docs copy a paragraph of topic %q: %q", name, normalized)
			}
		}
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "internal/guide/topics/") {
		t.Fatal("README does not link to the canonical guide topics")
	}
}
