package agent

import (
	"strings"
	"testing"
)

func TestLastMarkerWins(t *testing.T) {
	text := "REFERENCE: tn/core/x#1\nnoise\nREFERENCE: tn/core/tradernet#42546\n"
	if got := Reference(text); got != "tn/core/tradernet#42546" {
		t.Fatalf("got %q", got)
	}
	if got := Reference("no markers here"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestQuestions(t *testing.T) {
	qs, present, err := Questions(`QUESTIONS_JSON: [{"question":"q1","options":["a","b"],"allow_custom":true},{"type":"decompose","question":"q2","options":["x"]}]`)
	if err != nil || !present || len(qs) != 2 {
		t.Fatalf("present=%v err=%v qs=%v", present, err, qs)
	}
	if qs[0].Type != "business" || qs[1].Type != "decompose" {
		t.Fatalf("types: %q %q", qs[0].Type, qs[1].Type)
	}

	if _, present, _ := Questions("plain text"); present {
		t.Fatal("marker misdetected")
	}
	if _, present, err := Questions("QUESTIONS_JSON: not-json"); !present || err == nil {
		t.Fatal("invalid JSON must be an error")
	}
}

func TestStripMarkers(t *testing.T) {
	in := "Инструкция запуска:\n\n```sh\ngo run .\n```\n\nBRANCH_DESCRIPTION: how-to-run-answer\n\nDECOMPOSE_RESULT: no-split\n"
	got := StripMarkers(in)
	if strings.Contains(got, "BRANCH_DESCRIPTION") || strings.Contains(got, "DECOMPOSE_RESULT") {
		t.Errorf("markers leaked into chat text: %q", got)
	}
	if !strings.Contains(got, "go run .") {
		t.Errorf("answer body lost: %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("trailing blank lines left: %q", got)
	}
	// Чанк только из маркеров показывать нечего.
	if StripMarkers("REFERENCE: task-1\nQUESTIONS_JSON: []\n") != "" {
		t.Error("marker-only chunk must be empty")
	}
	// Полный текст парсится по-прежнему (маркеры снимаются только для UI).
	if BranchSlug(in) != "how-to-run-answer" {
		t.Errorf("marker parsing broken: %q", BranchSlug(in))
	}
}

// Вердикт ревью плана: parsed для раннего выхода, но в чат не попадает.
func TestPlanReviewMarker(t *testing.T) {
	text := "Проверил план по коду, находок нет.\n\nPLAN_REVIEW: clean\n"
	if got := PlanReview(text); got != "clean" {
		t.Fatalf("got %q", got)
	}
	if got := PlanReview("нет маркера"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if strings.Contains(StripMarkers(text), "PLAN_REVIEW") {
		t.Error("PLAN_REVIEW leaked into chat text")
	}
}

// Вердикт плана: parsed для пропуска execute/review, в чат не попадает.
func TestPlanResultMarker(t *testing.T) {
	if got := PlanResult("итог\n\nPLAN_RESULT: no-code\n"); got != "no-code" {
		t.Fatalf("got %q", got)
	}
	if got := PlanResult("без маркера"); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if strings.Contains(StripMarkers("ответ\nPLAN_RESULT: code\n"), "PLAN_RESULT") {
		t.Error("PLAN_RESULT leaked into chat text")
	}
}
