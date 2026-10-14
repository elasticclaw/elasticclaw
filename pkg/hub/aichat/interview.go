package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type InterviewAnswers struct {
	QuestionSetID string                 `json:"questionSetId"`
	Steps         []InterviewStepAnswers `json:"steps"`
}
type InterviewStepAnswers struct {
	Skipped bool              `json:"skipped"`
	Answers []InterviewAnswer `json:"answers"`
}
type InterviewAnswer struct {
	QuestionID string   `json:"questionId"`
	Choices    []string `json:"choices"`
	Other      string   `json:"other,omitempty"`
}
type AnswersSummary struct {
	QuestionSetID string          `json:"questionSetId"`
	Title         string          `json:"title"`
	Answers       []AnswerSummary `json:"answers"`
	Skipped       int             `json:"skipped"`
}
type AnswerSummary struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

var ErrInterviewStale = errors.New("interview is no longer available")

func acceptInterview(ctx context.Context, tx *sql.Tx, threadID string, answers InterviewAnswers) (string, json.RawMessage, error) {
	var id, role, status string
	var raw sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,role,status,interview_json FROM ai_chat_messages WHERE thread_id=? ORDER BY seq DESC LIMIT 1`, threadID).Scan(&id, &role, &status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrInterviewStale
	}
	if err != nil {
		return "", nil, err
	}
	var interview tools.Interview
	if role != "assistant" || status != "completed" || json.Unmarshal([]byte(raw.String), &interview) != nil || interview.QuestionSetID == "" || interview.QuestionSetID != answers.QuestionSetID || interview.Answered {
		return "", nil, ErrInterviewStale
	}
	text, summary, err := summarizeAnswers(interview, answers)
	if err != nil {
		return "", nil, err
	}
	interview.Answered = true
	updated, _ := json.Marshal(interview)
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_messages SET interview_json=? WHERE id=?`, string(updated), id); err != nil {
		return "", nil, err
	}
	encoded, _ := json.Marshal(summary)
	return text, encoded, nil
}

func summarizeAnswers(interview tools.Interview, answers InterviewAnswers) (string, AnswersSummary, error) {
	summary := AnswersSummary{QuestionSetID: interview.QuestionSetID, Title: interview.Title, Answers: []AnswerSummary{}}
	invalid := func(message string) (string, AnswersSummary, error) {
		return "", AnswersSummary{}, tools.ArgError(message)
	}
	if len(answers.Steps) != len(interview.Steps) {
		return invalid("answer every step or skip it")
	}
	lines := []string{fmt.Sprintf("Answers to %q:", interview.Title)}
	for i, step := range interview.Steps {
		submitted := answers.Steps[i]
		if submitted.Skipped {
			if len(submitted.Answers) > 0 {
				return invalid("skipped steps cannot contain answers")
			}
			summary.Skipped++
			lines = append(lines, fmt.Sprintf("(Skipped step %d)", i+1))
			continue
		}
		if len(submitted.Answers) != len(step.Questions) {
			return invalid("answer every question or skip the step")
		}
		byID := map[string]InterviewAnswer{}
		for _, answer := range submitted.Answers {
			if _, exists := byID[answer.QuestionID]; exists {
				return invalid("duplicate question answer")
			}
			byID[answer.QuestionID] = answer
		}
		for _, q := range step.Questions {
			answer, ok := byID[q.ID]
			if !ok {
				return invalid("invalid question id")
			}
			other := strings.TrimSpace(answer.Other)
			if utf8.RuneCountInString(other) > 300 {
				return invalid("other answer exceeds 300 characters")
			}
			if len(answer.Choices) == 0 && other == "" {
				return invalid("choose an option or enter an answer")
			}
			if q.Kind == "single" && (len(answer.Choices) > 1 || (len(answer.Choices) > 0 && other != "")) {
				return invalid("single questions accept one choice or other")
			}
			labels := []string{}
			for _, o := range q.Options {
				labels = append(labels, o.Label)
			}
			selected := []string{}
			for _, choice := range answer.Choices {
				if !slices.Contains(labels, choice) || slices.Contains(selected, choice) {
					return invalid("invalid or duplicate choice")
				}
				selected = append(selected, choice)
			}
			if other != "" {
				selected = append(selected, other)
			}
			display := strings.Join(selected, ", ")
			name := q.Label
			if name == "" {
				name = q.Prompt
			}
			summary.Answers = append(summary.Answers, AnswerSummary{Question: name, Answer: display})
			lines = append(lines, "- "+q.Prompt+" "+display)
		}
	}
	return strings.Join(lines, "\n"), summary, nil
}
