package azuredevopstest

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// Update is one entry of a work item's update history: who changed the tag
// list, and its value before and after.
type Update struct {
	// RevisedBy is the unique name of the identity that made the change.
	RevisedBy string
	// OldTags and NewTags are the System.Tags values around the change.
	OldTags, NewTags string
}

// FactoryIdentity is the identity that az devops login signs in; the
// scripted CLI records it as the author of every tag write that Factory
// makes.
const FactoryIdentity = "factory@contoso.com"

// AddTags adds tags to a work item as author does in the Azure DevOps UI,
// and records the change in the update history, so an agent-ready tag has an
// author that the adapter can check.
func (b *Boards) AddTags(id int, author string, tags ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.itemIndex(id)
	if index < 0 {
		b.t.Fatalf("AddTags: work item %d does not exist", id)
	}
	b.setTags(index, author, strings.Join(append(b.items[index].Tags(), tags...), "; "))
}

// SetTags replaces the tag list of a work item as author does, for example
// to reassign it to another repository.
func (b *Boards) SetTags(id int, author string, tags ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.itemIndex(id)
	if index < 0 {
		b.t.Fatalf("SetTags: work item %d does not exist", id)
	}
	b.setTags(index, author, strings.Join(tags, "; "))
}

// itemIndex returns the index of a work item, or -1. The caller holds b.mu.
func (b *Boards) itemIndex(id int) int {
	return slices.IndexFunc(b.items, func(item Item) bool { return item.ID == id })
}

// setTags writes the complete tag value and records the update. The caller
// holds b.mu.
func (b *Boards) setTags(index int, author, value string) {
	old, _ := b.items[index].Fields["System.Tags"].(string)
	b.items[index].Fields["System.Tags"] = value
	b.items[index].Updates = append(b.items[index].Updates, Update{RevisedBy: author, OldTags: old, NewTags: value})
}

// extensionUpdates answers the invoke wit/updates read of one work item.
func (b *Boards) extensionUpdates(call ExtensionCall) (any, error) {
	id, _ := strconv.Atoi(call.Parameters("--route-parameters")["id"])
	index := b.itemIndex(id)
	if index < 0 {
		return nil, errors.New("ERROR: TF401232: Work item " + strconv.Itoa(id) + " does not exist")
	}
	skip, _ := strconv.Atoi(call.Parameters("--query-parameters")["$skip"])
	values := []map[string]any{}
	for _, update := range b.items[index].Updates[min(skip, len(b.items[index].Updates)):] {
		values = append(values, map[string]any{
			"revisedBy": map[string]string{"uniqueName": update.RevisedBy},
			"fields":    map[string]any{"System.Tags": map[string]string{"oldValue": update.OldTags, "newValue": update.NewTags}},
		})
	}
	return map[string]any{"count": len(values), "value": values}, nil
}

// extensionUpdateTags answers az boards work-item update --fields
// System.Tags=...: it replaces the complete tag value, as Azure DevOps does,
// so a write that leaves a tag out deletes it.
func (b *Boards) extensionUpdateTags(call ExtensionCall) (any, error) {
	id, _ := strconv.Atoi(call.Flag("--id"))
	index := b.itemIndex(id)
	if index < 0 {
		return nil, errors.New("ERROR: TF401232: Work item " + strconv.Itoa(id) + " does not exist")
	}
	for _, pair := range call.Flags["--fields"] {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name != "System.Tags" {
			b.t.Errorf("work-item update sets %q, want only System.Tags", pair)
			continue
		}
		b.setTags(index, FactoryIdentity, value)
	}
	return b.itemJSON(b.items[index]), nil
}

// Comment is one work item comment of the scripted project.
type Comment struct {
	ID     int
	Text   string
	Author string
}

// extensionComments answers the invoke wit/comments routes: GET lists every
// comment on one page, POST creates a Markdown comment, and PATCH edits one.
// Factory writes as FactoryIdentity.
func (b *Boards) extensionComments(call ExtensionCall) (any, error) {
	route := call.Parameters("--route-parameters")
	id, _ := strconv.Atoi(route["workItemId"])
	index := b.itemIndex(id)
	if index < 0 {
		return nil, errors.New("ERROR: TF401232: Work item " + strconv.Itoa(id) + " does not exist")
	}
	item := &b.items[index]
	var body struct {
		Text string `json:"text"`
	}
	if call.Body != "" {
		if err := json.Unmarshal([]byte(call.Body), &body); err != nil {
			b.t.Errorf("decode comment body %q: %v", call.Body, err)
		}
	}
	switch call.Flag("--http-method") {
	case "POST":
		item.Comments = append(item.Comments, Comment{ID: len(item.Comments) + 1, Text: body.Text, Author: FactoryIdentity})
		return commentJSON(item.Comments[len(item.Comments)-1]), nil
	case "PATCH":
		commentID, _ := strconv.Atoi(route["commentId"])
		for position := range item.Comments {
			if item.Comments[position].ID == commentID {
				item.Comments[position].Text = body.Text
				return commentJSON(item.Comments[position]), nil
			}
		}
		return nil, errors.New("ERROR: comment " + route["commentId"] + " does not exist")
	}
	comments := []map[string]any{}
	for _, comment := range item.Comments {
		comments = append(comments, commentJSON(comment))
	}
	return map[string]any{"comments": comments}, nil
}

// commentJSON renders one comment as the comments resource returns it.
func commentJSON(comment Comment) map[string]any {
	return map[string]any{
		"id": comment.ID, "text": comment.Text, "format": "markdown",
		"createdBy": map[string]string{"uniqueName": comment.Author}, "createdDate": "2026-11-16T09:00:00Z",
	}
}
