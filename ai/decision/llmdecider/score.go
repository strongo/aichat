package llmdecider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/strongo/aichat/ai"
	"github.com/strongo/aichat/ai/decision"
)

var _ decision.ScoredProvider = (*Decider)(nil)

// ErrBadScores is returned (wrapped) when the model's answer to a Score call is
// not usable. Its text never quotes the model's output or the request.
var ErrBadScores = errors.New("llmdecider: unusable scores")

// scoreSystemPrompt is product-neutral. The model's numbers are its own
// estimates, which is why the answer is marked uncalibrated.
const scoreSystemPrompt = `You score candidates for a conversational product. The user message is a JSON object with the text to judge ("question"), optional "context", and a list of "questions". Answer EVERY question with one probability in [0,1] for EVERY candidate id of that question.
- kind "choice": exactly one candidate is correct. The probabilities must sum to 1. If a "noneId" is given and no other candidate fits, put the probability on it.
- kind "relevance": judge each candidate on its own. The probabilities are independent and need not sum to 1.
Use only the candidate ids given; never invent ids. Be honest about uncertainty: use low probabilities for candidates that are not clearly right.`

// scorePrompt renders the request as the user message.
type scorePrompt struct {
	Question  string              `json:"question"`
	Context   map[string]any      `json:"context,omitempty"`
	Questions []decision.Question `json:"questions"`
}

type wireScores struct {
	Answers []struct {
		QuestionID string `json:"questionId"`
		Scores     []struct {
			ID          string  `json:"id"`
			Probability float64 `json:"probability"`
		} `json:"scores"`
	} `json:"answers"`
}

// Score implements decision.ScoredProvider with ONE structured inference for
// every question in req. The answers are the model's own estimates: they are
// never Calibrated and carry no confidence (HasConfidence is false), so a
// decision.SelectionPolicy reads them as a proposal rather than a selection.
//
// A KindChoice answer is normalised to sum to 1 (a model's own numbers rarely
// do); candidates the model left out score 0 and ids it invented are dropped. A
// question the model did not answer, or a choice it gave no mass at all, is an
// error (ErrBadScores). The result is valid by construction (known candidates,
// probabilities in [0,1], a choice summing to 1: decision.ValidateScoreResult
// passes, which the tests assert). ScoreResult.Model is the configured Options.Model ("" when
// the provider chooses).
func (d *Decider) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	if err := decision.ValidateScoreRequest(req); err != nil {
		// Not %w of err: its text quotes the caller's ids.
		return decision.ScoreResult{}, fmt.Errorf("llmdecider: %w: the score request failed validation", decision.ErrInvalidRequest)
	}
	payload, err := json.Marshal(scorePrompt{Question: req.Text, Context: req.Context, Questions: req.Questions})
	if err != nil {
		return decision.ScoreResult{}, fmt.Errorf("llmdecider: %w: the score request cannot be encoded", decision.ErrInvalidRequest)
	}
	raw, usage, err := d.infer(ctx, ai.ChatRequest{
		Model:          d.opts.Model,
		System:         scoreSystemPrompt,
		Messages:       []ai.Message{{Role: ai.RoleUser, Text: string(payload)}},
		ResponseSchema: json.RawMessage(scoreSchema),
		Metadata:       map[string]string{"path": "score"},
	})
	if err != nil {
		return decision.ScoreResult{}, err
	}
	var w wireScores
	if err := json.Unmarshal(raw, &w); err != nil {
		return decision.ScoreResult{}, fmt.Errorf("%w: not JSON of the documented shape", ErrBadScores)
	}
	res := decision.ScoreResult{Engine: d.opts.Name, Model: d.opts.Model, Answers: map[string]decision.Answer{}}
	if usage != nil {
		res.Usage = decision.Usage{InputTokens: int(usage.InputTokens), OutputTokens: int(usage.OutputTokens)}
	}
	for qi, q := range req.Questions {
		ans, err := foldScores(qi, q, w)
		if err != nil {
			return decision.ScoreResult{}, err
		}
		res.Answers[q.ID] = ans
	}
	return res, nil
}

// foldScores builds the Answer for question qi (named by position in errors,
// never by id).
func foldScores(qi int, q decision.Question, w wireScores) (decision.Answer, error) {
	probs := map[string]float64{}
	found := false
	for _, a := range w.Answers {
		if a.QuestionID != q.ID {
			continue
		}
		found = true
		for _, s := range a.Scores {
			if _, dup := probs[s.ID]; !dup {
				probs[s.ID] = clamp01(s.Probability)
			}
		}
		break
	}
	if !found {
		return decision.Answer{}, fmt.Errorf("%w: no answer for question %d", ErrBadScores, qi)
	}
	scores := make([]decision.Score, 0, len(q.Candidates))
	sum := 0.0
	for _, c := range q.Candidates { // known candidates only; missing ones score 0
		scores = append(scores, decision.Score{ID: c.ID, Probability: probs[c.ID]})
		sum += probs[c.ID]
	}
	if q.Kind == decision.KindChoice {
		if sum <= 0 {
			return decision.Answer{}, fmt.Errorf("%w: question %d has no probability on any candidate", ErrBadScores, qi)
		}
		for i := range scores {
			scores[i].Probability /= sum
		}
	}
	ans := decision.NewAnswer(q.ID, q.Kind, scores)
	if q.Kind == decision.KindChoice {
		ans.NoneID = q.NoneID
	}
	return ans, nil
}

// clamp01 maps a model's number into [0,1].
func clamp01(p float64) float64 { return min(max(p, 0), 1) }
