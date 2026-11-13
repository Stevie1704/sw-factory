package factory

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Stevie1704/sw-factory/internal/store"
	"github.com/Stevie1704/sw-factory/internal/tracker"
)

// scopedCommentPrefix versions one incoming comment's origin-aware identity.
const scopedCommentPrefix = "factory-comment:v1:"

// commentWatermarksPrefix versions the two-stream payload in the existing field.
// Legacy tracker payloads and raw watermark values retain their original shape.
const commentWatermarksPrefix = "factory-comment-watermarks:v1:"

// separateIssueCommentStream resolves the optional adapter namespace capability.
func (s *Service) separateIssueCommentStream() bool {
	capability, ok := s.deps.Tracker.(tracker.SeparateCommentStreams)
	return ok && capability.SeparateIssueCommentStream()
}

// commandStream validates an explicit origin, including equal numeric targets.
func commandStream(run store.Run, request CommandRequest) (string, error) {
	stream := request.Stream
	if stream == "" {
		if run.PullRequestNumber > 0 && run.IssueNumber == run.PullRequestNumber {
			return "", &PolicyRejection{Code: PolicyRejectionWrongTarget, Problem: "issue and pull request have the same number; the command must name its comment stream"}
		}
		if request.IssueNumber == run.IssueNumber {
			stream = "issue"
		} else {
			stream = "pull_request"
		}
	}
	if stream == "issue" && request.IssueNumber == run.IssueNumber || stream == "pull_request" && request.IssueNumber == run.PullRequestNumber && run.PullRequestNumber > 0 {
		return stream, nil
	}
	return "", &PolicyRejection{Code: PolicyRejectionWrongTarget, Problem: fmt.Sprintf("comment stream %q does not match its displayed Run target", stream)}
}

// scopedCommentID marks the origin without allowing authored text to define it.
func scopedCommentID(stream, id string) string {
	return scopedCommentPrefix + stream + ":" + base64.RawURLEncoding.EncodeToString([]byte(id))
}

// splitScopedCommentID decodes only coordinator-created stream identities.
func splitScopedCommentID(id string) (string, string, bool) {
	if !strings.HasPrefix(id, scopedCommentPrefix) {
		return "", "", false
	}
	stream, encoded, ok := strings.Cut(strings.TrimPrefix(id, scopedCommentPrefix), ":")
	if !ok || (stream != "issue" && stream != "pull_request" && stream != "local_operator") {
		return "", "", false
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 {
		return "", "", false
	}
	return stream, string(value), true
}

// commentWatermarks reads versioned local streams while preserving legacy issue IDs.
func commentWatermarks(value string) map[string]string {
	result := map[string]string{}
	if strings.HasPrefix(value, commentWatermarksPrefix) {
		encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, commentWatermarksPrefix))
		if err == nil {
			_ = json.Unmarshal(encoded, &result)
		}
	} else if value != "" {
		result["issue"] = value
	}
	return result
}

// mergeCommentWatermark advances only the originating stream's watermark.
func mergeCommentWatermark(previous, current string) string {
	stream, id, ok := splitScopedCommentID(current)
	if !ok {
		return current
	}
	watermarks := commentWatermarks(previous)
	if last := watermarks[stream]; last == "" || compareEventIDs(last, id) < 0 {
		watermarks[stream] = id
	}
	encoded, _ := json.Marshal(watermarks)
	return commentWatermarksPrefix + base64.RawURLEncoding.EncodeToString(encoded)
}

// localOperatorReceipt is the latest local stream's durable disposition. Remote
// projections preserve it even when they replace the Run's last-command fields.
type localOperatorReceipt struct{ ID, Outcome, Detail string }

// localOperatorReceiptKey keeps receipt metadata outside the ordered stream IDs.
const localOperatorReceiptKey = "local_operator_receipt"

// recordLocalOperatorReceipt commits a local disposition with its Run transition.
func recordLocalOperatorReceipt(watermarks, commentID, outcome, detail string) string {
	stream, id, ok := splitScopedCommentID(commentID)
	if !ok || stream != "local_operator" {
		return watermarks
	}
	values := commentWatermarks(watermarks)
	receipt, _ := json.Marshal(localOperatorReceipt{ID: id, Outcome: outcome, Detail: store.TruncateUTF8(detail, 1024)})
	values[localOperatorReceiptKey] = string(receipt)
	data, _ := json.Marshal(values)
	return commentWatermarksPrefix + base64.RawURLEncoding.EncodeToString(data)
}

// replayLocalOperatorReceipt restores its own outcome without reapplying a handler.
func replayLocalOperatorReceipt(run store.Run, commentID string) (store.IdeationCommandOutcome, string) {
	_, id, _ := splitScopedCommentID(commentID)
	var receipt localOperatorReceipt
	if json.Unmarshal([]byte(commentWatermarks(run.ProcessedCommentID)[localOperatorReceiptKey]), &receipt) != nil || receipt.ID != id {
		return store.IdeationCommandConflict, "local command was processed; its exact disposition is unavailable, inspect Run history before a new request"
	}
	if receipt.Outcome == string(CommandRejected) {
		return store.IdeationCommandRefused, receipt.Detail
	}
	return store.IdeationCommandApplied, receipt.Detail
}
