package typesafe

import (
	"context"
	"fmt"

	"github.com/strongo/aichat/ai/decision"
)

var (
	_ decision.Provider       = (*Client)(nil)
	_ decision.ScoredProvider = (*Client)(nil)
)

// scoreState is the state of a ScoreRequest: an object holding the question
// text, the context, and the candidates of every relevance question. The
// candidate catalogue lives in the state, not in each question, because the
// model reads a shared, structured catalogue far better than a description
// repeated inside every yes/no question (measured against the live API on a
// catalogue of 11 tables: with per-question descriptions every candidate scored
// 0.2-0.4 and nothing separated; with the catalogue in the state the relevant
// ones scored 0.8+ against 0.05).
func scoreState(req decision.ScoreRequest) map[string]any {
	st := map[string]any{"question": req.Text}
	if len(req.Context) > 0 {
		st["context"] = req.Context
	}
	catalogue := map[string]any{}
	for qi, q := range req.Questions {
		if q.Kind != decision.KindRelevance {
			continue
		}
		list := make([]map[string]any, len(q.Candidates))
		for ci, cand := range q.Candidates {
			list[ci] = map[string]any{"name": cand.ID}
			if cand.Description != "" {
				list[ci]["description"] = cand.Description
			}
		}
		catalogue[choiceKey(qi)] = list
	}
	if len(catalogue) > 0 {
		st["candidates"] = catalogue
	}
	return st
}

// candidateRef locates a relevance candidate's answer.
type candidateRef struct {
	question int
	id       string
}

// Score implements decision.ScoredProvider with ONE API call for every
// question in req.
//
// A KindChoice question becomes one Choice (one option per candidate). A
// KindRelevance question becomes one Noul per candidate: a Choice's
// probabilities sum to 1, so "which of these tables are relevant" cannot be
// asked as a Choice without relevant tables splitting the probability between
// them. A Noul has no confidence, so a relevance Answer has none either.
func (c *Client) Score(ctx context.Context, req decision.ScoreRequest) (decision.ScoreResult, error) {
	if err := decision.ValidateScoreRequest(req); err != nil {
		// Not %w of err: its text quotes the caller's question and candidate ids.
		return decision.ScoreResult{}, fmt.Errorf("%w: the score request failed validation", ErrInvalidRequest)
	}
	questions := map[string]Question{}
	refs := map[string]candidateRef{} // wire key -> candidate (relevance only)
	for qi, q := range req.Questions {
		switch q.Kind {
		case decision.KindChoice:
			opts := make(map[string]any, len(q.Candidates))
			for _, cand := range q.Candidates {
				if cand.Description == "" {
					opts[cand.ID] = nil
				} else {
					opts[cand.ID] = cand.Description
				}
			}
			questions[choiceKey(qi)] = Choice(q.Instructions+" The question is `state.question`.", opts)
		default: // decision.KindRelevance, the only other kind ValidateScoreRequest lets through
			for ci, cand := range q.Candidates {
				key := relevanceKey(qi, ci)
				refs[key] = candidateRef{question: qi, id: cand.ID}
				questions[key] = Noul(fmt.Sprintf("%s Judge the candidate `state.candidates.%s[%d]` against the question in `state.question`.",
					q.Instructions, choiceKey(qi), ci))
			}
		}
	}

	resp, err := c.Ask(ctx, AskRequest{State: scoreState(req), Questions: questions})
	if err != nil {
		return decision.ScoreResult{}, err
	}
	res := decision.ScoreResult{Engine: c.cfg.Name, Model: resp.Model, Usage: decision.Usage(resp.Usage), Answers: map[string]decision.Answer{}}
	for qi, q := range req.Questions {
		var ans decision.Answer
		var err error
		if q.Kind == decision.KindChoice {
			ans, err = foldChoice(qi, q, resp.Answers[choiceKey(qi)])
		} else {
			ans, err = foldRelevance(qi, q, resp.Answers)
		}
		if err != nil {
			return decision.ScoreResult{}, err
		}
		res.Answers[q.ID] = ans
	}
	if err := decision.ValidateScoreResult(req, res); err != nil {
		return decision.ScoreResult{}, fmt.Errorf("%w: %s", ErrBadResponse, decision.InvalidDetail(err))
	}
	return res, nil
}

func choiceKey(qi int) string        { return fmt.Sprintf("q%d", qi) }
func relevanceKey(qi, ci int) string { return fmt.Sprintf("q%d.c%d", qi, ci) }

// foldChoice and foldRelevance name a question by its index, never its id: an
// error must not quote caller-supplied content.
func foldChoice(qi int, q decision.Question, a Answer) (decision.Answer, error) {
	if a.Type != TypeChoice || a.Probabilities == nil || a.Confidence == nil {
		return decision.Answer{}, fmt.Errorf("%w: question %d: expected a choice answer", ErrBadResponse, qi)
	}
	scores := make([]decision.Score, 0, len(a.Probabilities))
	for id, p := range a.Probabilities {
		scores = append(scores, decision.Score{ID: id, Probability: p})
	}
	out := decision.NewAnswer(q.ID, decision.KindChoice, scores)
	out.Confidence, out.HasConfidence, out.Calibrated, out.NoneID = *a.Confidence, true, true, q.NoneID
	return out, nil
}

func foldRelevance(qi int, q decision.Question, answers map[string]Answer) (decision.Answer, error) {
	scores := make([]decision.Score, 0, len(q.Candidates))
	for ci, cand := range q.Candidates {
		a, ok := answers[relevanceKey(qi, ci)]
		if !ok || a.Type != TypeNoul {
			return decision.Answer{}, fmt.Errorf("%w: question %d: expected a noul answer for candidate %d", ErrBadResponse, qi, ci)
		}
		scores = append(scores, decision.Score{ID: cand.ID, Probability: a.Noul})
	}
	out := decision.NewAnswer(q.ID, decision.KindRelevance, scores)
	out.Calibrated = true
	return out, nil
}
