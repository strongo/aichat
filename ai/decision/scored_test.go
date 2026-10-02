package decision

import (
	"strings"
	"testing"
)

func TestNewAnswer_SortsByProbabilityThenID(t *testing.T) {
	in := []Score{{"b", 0.2}, {"c", 0.5}, {"a", 0.2}}
	a := NewAnswer("q", KindChoice, in)
	got := []string{a.Scores[0].ID, a.Scores[1].ID, a.Scores[2].ID}
	if strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("order = %v", got)
	}
	if in[0].ID != "b" {
		t.Fatal("input slice was modified")
	}
	if a.QuestionID != "q" || a.Kind != KindChoice {
		t.Fatalf("answer = %+v", a)
	}
}

func TestAnswer_Probability(t *testing.T) {
	a := NewAnswer("q", KindChoice, []Score{{"x", 0.7}, {"y", 0.3}})
	if p, ok := a.Probability("y"); !ok || p != 0.3 {
		t.Fatalf("y = %v %v", p, ok)
	}
	if _, ok := a.Probability("zzz"); ok {
		t.Fatal("unknown candidate reported as scored")
	}
}

func goodRequest() ScoreRequest {
	return ScoreRequest{
		Text: "which table?",
		Questions: []Question{
			{ID: "q1", Kind: KindChoice, NoneID: "none", Candidates: []Candidate{{ID: "a"}, {ID: "b"}, {ID: "none"}}},
			{ID: "q2", Kind: KindRelevance, Candidates: []Candidate{{ID: "a"}, {ID: "b"}}},
		},
	}
}

func TestValidateScoreRequest(t *testing.T) {
	if err := ValidateScoreRequest(goodRequest()); err != nil {
		t.Fatalf("good request rejected: %v", err)
	}
	cases := map[string]func(*ScoreRequest){
		"no questions":         func(r *ScoreRequest) { r.Questions = nil },
		"question id":          func(r *ScoreRequest) { r.Questions[0].ID = "" },
		"duplicate question":   func(r *ScoreRequest) { r.Questions[1].ID = "q1" },
		"unknown kind":         func(r *ScoreRequest) { r.Questions[0].Kind = "weird" },
		"no candidates":        func(r *ScoreRequest) { r.Questions[1].Candidates = nil },
		"candidate id":         func(r *ScoreRequest) { r.Questions[1].Candidates[0].ID = "" },
		"duplicate candidate":  func(r *ScoreRequest) { r.Questions[1].Candidates[1].ID = "a" },
		"none not a candidate": func(r *ScoreRequest) { r.Questions[0].NoneID = "ghost" },
		"none on relevance":    func(r *ScoreRequest) { r.Questions[1].NoneID = "a" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := goodRequest()
			mutate(&r)
			if err := ValidateScoreRequest(r); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func goodResult() ScoreResult {
	return ScoreResult{Answers: map[string]Answer{
		"q1": {QuestionID: "q1", Kind: KindChoice, Scores: []Score{{"a", 0.6}, {"b", 0.3}, {"none", 0.1}}, Confidence: 0.5, HasConfidence: true},
		"q2": {QuestionID: "q2", Kind: KindRelevance, Scores: []Score{{"a", 0.9}, {"b", 0.8}}},
	}}
}

func TestValidateScoreResult(t *testing.T) {
	if err := ValidateScoreResult(goodRequest(), goodResult()); err != nil {
		t.Fatalf("good result rejected: %v", err)
	}
	cases := map[string]func(*ScoreResult){
		"missing answer":    func(r *ScoreResult) { delete(r.Answers, "q2") },
		"unknown candidate": func(r *ScoreResult) { r.Answers["q2"] = Answer{Scores: []Score{{"zzz", 0.5}}} },
		"out of range":      func(r *ScoreResult) { r.Answers["q2"] = Answer{Scores: []Score{{"a", 1.5}}} },
		"nan":               func(r *ScoreResult) { r.Answers["q2"] = Answer{Scores: []Score{{"a", nan()}}} },
		"choice not 1":      func(r *ScoreResult) { r.Answers["q1"] = Answer{Scores: []Score{{"a", 0.2}, {"b", 0.2}}} },
		"confidence range": func(r *ScoreResult) {
			r.Answers["q1"] = Answer{Scores: []Score{{"a", 1}}, Confidence: 2, HasConfidence: true}
		},
		"confidence nan": func(r *ScoreResult) {
			r.Answers["q1"] = Answer{Scores: []Score{{"a", 1}}, Confidence: nan(), HasConfidence: true}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := goodResult()
			mutate(&r)
			if err := ValidateScoreResult(goodRequest(), r); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	// A choice with no scores at all is tolerated by the sum check (the policy
	// reports it as uncertain), and a relevance sum is unconstrained.
	r := goodResult()
	r.Answers["q1"] = Answer{QuestionID: "q1", Kind: KindChoice}
	if err := ValidateScoreResult(goodRequest(), r); err != nil {
		t.Fatalf("empty choice rejected: %v", err)
	}
}
