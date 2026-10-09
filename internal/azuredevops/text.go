package azuredevops

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rich-text fields and comments in Azure DevOps are HTML unless the field or
// comment uses the Markdown format. The factory reads issue bodies and
// commands as text, so htmlText defines one conversion (ADR 0019).
var (
	htmlComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlLineBreak  = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlBlockEnd   = regexp.MustCompile(`(?i)</(div|p|li|h[1-6]|tr|pre|blockquote)\s*>`)
	htmlListItem   = regexp.MustCompile(`(?i)<li(\s[^>]*)?>`)
	htmlTag        = regexp.MustCompile(`(?s)<[^>]*>`)
	repeatedBreaks = regexp.MustCompile(`\n{3,}`)
)

// htmlText converts HTML to plain text. Block ends and line breaks become new
// lines, list items start with "- ", other tags are removed, and entities are
// decoded. HTML comments stay, so a factory marker such as
// `<!-- factory-route: fix -->` survives whether the author typed it as text
// or the description stores it as a comment.
func htmlText(value string) string {
	comments := []string{}
	text := htmlComment.ReplaceAllStringFunc(value, func(comment string) string {
		comments = append(comments, comment)
		return fmt.Sprintf("\x00%d\x00", len(comments)-1)
	})
	text = htmlLineBreak.ReplaceAllString(text, "\n")
	text = htmlBlockEnd.ReplaceAllString(text, "\n")
	text = htmlListItem.ReplaceAllString(text, "- ")
	text = htmlTag.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	for index, comment := range comments {
		text = strings.Replace(text, fmt.Sprintf("\x00%d\x00", index), "\n"+comment+"\n", 1)
	}
	text = strings.ReplaceAll(text, " ", " ")
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " \t")
	}
	text = strings.Join(lines, "\n")
	text = repeatedBreaks.ReplaceAllString(text, "\n\n")
	text = strings.ReplaceAll(text, "\n\n<!--", "\n<!--")
	return strings.TrimSpace(text)
}

// richText returns the text of a rich-text value in the reported format.
func richText(value, format string) string {
	if strings.EqualFold(strings.TrimSpace(format), "markdown") {
		return value
	}
	return htmlText(value)
}

// splitTags converts the System.Tags field ("a; b") into labels.
func splitTags(value string) []string {
	labels := make([]string, 0)
	for _, tag := range strings.Split(value, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			labels = append(labels, tag)
		}
	}
	return labels
}

// joinTags converts labels into the System.Tags field value.
func joinTags(labels []string) string {
	return strings.Join(labels, "; ")
}

// eventIDTime is the fixed-width UTC time that starts every comment and
// review identity. The coordinator orders identities that are not numbers as
// text, so this prefix makes them sort in creation order across work-item
// comments and pull-request threads (ADR 0019).
const eventIDTime = "20060102T150405.000000000Z"

// idPart is one tagged number of an event identity, for example work item
// 42 as {"w", 42}.
type idPart struct {
	tag    string
	number int
}

// eventID returns an identity that sorts by created and names its source by
// the tagged numbers, for example ".w42.c7" for comment 7 of work item 42.
func eventID(created time.Time, parts ...idPart) string {
	var builder strings.Builder
	builder.WriteString(created.UTC().Format(eventIDTime))
	for _, part := range parts {
		fmt.Fprintf(&builder, ".%s%d", part.tag, part.number)
	}
	return builder.String()
}

// eventIDPart returns the number tagged with prefix in an identity made by
// eventID.
func eventIDPart(id, prefix string) (int, bool) {
	segments := strings.Split(id, ".")
	if len(segments) < 3 {
		return 0, false
	}
	for _, segment := range segments[2:] {
		if !strings.HasPrefix(segment, prefix) {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(segment, prefix))
		if err != nil || number <= 0 {
			return 0, false
		}
		return number, true
	}
	return 0, false
}
