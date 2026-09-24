package agent

import (
	"encoding/json"
	"strings"
)

// Markers extracted from the assistant's accumulated text. Per spec 05:
// scan the full assembled text, last occurrence wins.

type QuestionSpec struct {
	Type        string   `json:"type"` // business (default) | decompose
	Question    string   `json:"question"`
	Options     []string `json:"options"`
	AllowCustom bool     `json:"allow_custom"`
}

// LastMarker returns the value after the last occurrence of `prefix` at a
// line start, or "".
func LastMarker(text, prefix string) string {
	var val string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			val = strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return val
}

func Reference(text string) string    { return LastMarker(text, "REFERENCE:") }
func BranchSlug(text string) string   { return LastMarker(text, "BRANCH_DESCRIPTION:") }
func DecomposeRes(text string) string { return LastMarker(text, "DECOMPOSE_RESULT:") }
func Triage(text string) string       { return LastMarker(text, "TRIAGE:") }
func PlanReview(text string) string   { return LastMarker(text, "PLAN_REVIEW:") }
func PlanResult(text string) string   { return LastMarker(text, "PLAN_RESULT:") }

// markerPrefixes are protocol lines between the agent and the orchestrator.
// They are parsed from the full text, but must not reach the chat: for a human
// reader they are noise at the end of an otherwise readable answer.
var markerPrefixes = []string{
	"REFERENCE:", "BRANCH_DESCRIPTION:", "DECOMPOSE_RESULT:", "QUESTIONS_JSON:", "TRIAGE:", "PLAN_REVIEW:", "PLAN_RESULT:",
}

// StripMarkers removes marker lines from a chunk of assistant text and trims
// the blank lines they leave behind. Returns "" if nothing human-readable is
// left (such a chunk is not worth showing at all). extra — маркеры манифеста
// скилла сверх встроенных.
func StripMarkers(text string, extra ...string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	prefixes := markerPrefixes
	if len(extra) > 0 {
		prefixes = append(append([]string(nil), markerPrefixes...), extra...)
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isMarker := false
		for _, p := range prefixes {
			if strings.HasPrefix(trimmed, p) {
				isMarker = true
				break
			}
		}
		if !isMarker {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// Questions parses the last QUESTIONS_JSON marker. The bool is false when the
// marker is absent; err != nil means the marker is present but invalid (stage
// error per spec 05).
func Questions(text string) ([]QuestionSpec, bool, error) {
	raw := LastMarker(text, "QUESTIONS_JSON:")
	if raw == "" {
		return nil, false, nil
	}
	var qs []QuestionSpec
	if err := json.Unmarshal([]byte(raw), &qs); err != nil {
		return nil, true, err
	}
	for i := range qs {
		if qs[i].Type == "" {
			qs[i].Type = "business"
		}
	}
	return qs, true, nil
}
