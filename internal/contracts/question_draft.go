package contracts

import (
	"errors"
	"reflect"
	"strings"

	"agent-platform/internal/contracts/queryinput"
)

var ErrQuestionDraftUnavailable = errors.New("question awaiting is no longer active")

func CloneQuestionDraft(draft *queryinput.QuestionDraft) *queryinput.QuestionDraft {
	if draft == nil {
		return nil
	}
	out := &queryinput.QuestionDraft{Revision: draft.Revision, Answers: make([]map[string]any, len(draft.Answers))}
	for i, answer := range draft.Answers {
		out.Answers[i] = CloneMap(answer)
		if values, ok := answer["answers"].([]string); ok {
			out.Answers[i]["answers"] = append([]string(nil), values...)
		}
	}
	return out
}

// QuestionDraftUpdater uses the existing awaiting lock. normalize is the
// toolinteraction single-item validator supplied by Runtime; it must be pure
// and must not call back into RunControl.
type QuestionDraftUpdater interface {
	UpdateQuestionDraft(runID, awaitingID string, answer map[string]any, normalize func(map[string]any, map[string]any) (any, error)) (*queryinput.QuestionDraft, error)
}

func (c *RunControl) UpdateQuestionDraft(awaitingID string, answer map[string]any, normalize func(map[string]any, map[string]any) (any, error)) (*queryinput.QuestionDraft, error) {
	if c == nil || normalize == nil {
		return nil, ErrQuestionDraftUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.resolveAwaitingAliasLocked(strings.TrimSpace(awaitingID))
	awaiting, ok := c.awaitingSubmits[id]
	if !ok || awaiting.Mode != "question" || c.finished.Load() || c.interrupted.Load() {
		return nil, ErrQuestionDraftUnavailable
	}
	questionID, _ := answer["id"].(string)
	if questionID == "" {
		return nil, errors.New("question id is required")
	}
	for key := range answer {
		if key != "id" && key != "answer" && key != "answers" {
			return nil, errors.New("invalid question answer field")
		}
	}
	var definition map[string]any
	for _, raw := range awaiting.Questions {
		item, _ := raw.(map[string]any)
		if item["id"] == questionID {
			if definition != nil {
				return nil, errors.New("duplicate question id")
			}
			definition = item
		}
	}
	if definition == nil {
		return nil, errors.New("unknown question id")
	}
	kind := strings.ToLower(strings.TrimSpace(AnyStringNode(definition["type"])))
	if kind == "password" {
		return nil, errors.New("password must be entered in the UI")
	}
	value, err := normalize(definition, answer)
	if err != nil {
		return nil, err
	}
	key := "answer"
	if kind == "multi-select" {
		key = "answers"
	}
	normalized := map[string]any{"id": questionID, key: value}
	draft := CloneQuestionDraft(awaiting.QuestionDraft)
	if draft == nil {
		draft = &queryinput.QuestionDraft{}
	}
	for _, existing := range draft.Answers {
		if existing["id"] == questionID {
			if reflect.DeepEqual(existing, normalized) {
				return draft, nil
			}
			return nil, errors.New("question already answered in voice draft")
		}
	}
	draft.Answers = append(draft.Answers, normalized)
	draft.Revision++
	awaiting.QuestionDraft = CloneQuestionDraft(draft)
	c.awaitingSubmits[id] = awaiting
	return draft, nil
}
