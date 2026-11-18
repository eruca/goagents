package goagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eruca/goagents/goagent/ports"
	"github.com/eruca/goagents/llmkit"
)

func requestNumber(n int) ports.ChatRequest {
	return ports.ChatRequest{Messages: []ports.ChatMessage{{Content: strconv.Itoa(n)}}}
}
func numericVerifier(_ context.Context, req ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
	n, err := strconv.Atoi(req.Messages[0].Content)
	if err != nil {
		return false, err
	}
	got, err := strconv.Atoi(r.Content)
	return err == nil && got == 2*n, nil
}
func verifierFixtures() []VerifierCase {
	return []VerifierCase{
		{ID: "correct", Request: requestNumber(7), Response: &ports.ChatResponse{Content: "14"}, ExpectedPassed: true},
		{ID: "wrong-number", Request: requestNumber(7), Response: &ports.ChatResponse{Content: "15"}},
		{ID: "missing-answer", Request: requestNumber(7), Response: &ports.ChatResponse{}},
		{ID: "format-only", Request: requestNumber(7), Response: &ports.ChatResponse{Content: "a fluent unsupported answer"}},
	}
}

func TestVerifierAuditDetectsFalseAcceptsAndFalseRejects(t *testing.T) {
	for _, kind := range []string{"correct", "format-only", "always-reject", "infrastructure", "panic"} {
		t.Run(kind, func(t *testing.T) {
			config := cascadeConfig()
			config.Tasks[0].Verify = numericVerifier
			switch kind {
			case "format-only":
				config.Tasks[0].Verify = func(_ context.Context, _ ports.ChatRequest, r *ports.ChatResponse) (bool, error) {
					_, err := strconv.Atoi(r.Content)
					return err == nil, nil
				}
			case "always-reject":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) { return false, nil }
			case "infrastructure":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) {
					return false, errors.New("private source text")
				}
			case "panic":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) {
					panic("private verifier failure")
				}
			}
			report, err := newCascade(t, config).AuditVerifier(context.Background(), cascadeKey, verifierFixtures())
			switch kind {
			case "correct":
				if err != nil || report == nil || !report.Passed || report.Positives != 1 || report.Negatives != 3 {
					t.Fatalf("report=%+v err=%v", report, err)
				}
			case "format-only":
				if !errors.Is(err, ErrVerifierRejected) || report == nil || report.FalseAccepts != 1 {
					t.Fatalf("missed semantic error: %+v %v", report, err)
				}
			case "always-reject":
				if !errors.Is(err, ErrVerifierRejected) || report == nil || report.FalseRejects != 1 {
					t.Fatalf("missed reject-all verifier: %+v %v", report, err)
				}
			case "infrastructure", "panic":
				if !errors.Is(err, ErrCascadeVerifier) || report != nil || strings.Contains(err.Error(), "private") {
					t.Fatalf("infra leak: %+v %v", report, err)
				}
			}
			for _, m := range config.Models {
				if len(m.Client.(*fakeProviderClient).requests) > 0 {
					t.Fatal("verifier audit invoked a provider")
				}
			}
		})
	}
}

type numericProvider struct {
	kind  string
	calls int
}

func (p *numericProvider) Chat(_ context.Context, req ports.ChatRequest) (*ports.ChatResponse, error) {
	p.calls++
	n, err := strconv.Atoi(req.Messages[0].Content)
	if err != nil {
		return nil, err
	}
	answer := 2 * n
	switch p.kind {
	case "cheap":
		if n%10 == 0 {
			answer++
		}
	case "higher-overall":
		if n%50 == 0 {
			answer++
		}
	case "rescue":
		if n%25 == 1 {
			answer++
		}
	}
	return &ports.ChatResponse{Content: strconv.Itoa(answer)}, nil
}

func evaluationConfig() CascadeConfig {
	c := cascadeConfig()
	c.Models = nil
	c.Evidence = nil
	c.Tasks[0].Verify = numericVerifier
	c.Policy = llmkit.QualityPolicy{MinSamples: 100, MaxAttempts: 3, MaxEstimatedCents: 4}
	for i, name := range []string{"cheap", "higher-overall", "rescue"} {
		cost := i
		price := llmkit.PriceLow
		if cost == 0 {
			price = llmkit.PriceFree
		}
		c.Models = append(c.Models, CascadeModel{Candidate: llmkit.Candidate{AccountAlias: "test", Model: llmkit.ModelCapability{Alias: name, Provider: "offline", PriceClass: price, EstimatedCents: cost, CapabilityLevel: llmkit.CapabilitySimple}}, Revision: "v1", Client: &numericProvider{kind: name}})
	}
	return c
}
func evaluationSet() CascadeEvaluationSet {
	set := CascadeEvaluationSet{VerifierCases: verifierFixtures()}
	for n := 1; n <= 100; n++ {
		set.Qualification = append(set.Qualification, QualityCase{ID: fmt.Sprintf("train-%d", n), Request: requestNumber(n)})
	}
	for n := 101; n <= 120; n++ {
		set.Validation = append(set.Validation, QualityCase{ID: fmt.Sprintf("holdout-%d", n), Request: requestNumber(n)})
	}
	return set
}
func evaluationOptions() CascadeEvaluationOptions {
	return CascadeEvaluationOptions{ExpiresAt: cascadeTime.Add(time.Hour), MaxEstimatedCents: 400, RescuePolicy: llmkit.RescuePolicy{MinSamples: 5, MinSuccessRate: 0.9}}
}

func TestCascadeEvaluationMeasuresRescueAndActualHoldoutChain(t *testing.T) {
	config := evaluationConfig()
	report, err := newCascade(t, config).EvaluateCascade(context.Background(), cascadeKey, evaluationSet(), evaluationOptions())
	if err != nil {
		t.Fatal(err)
	}
	rates := map[string]int{}
	for _, m := range report.Models {
		rates[m.Evidence.ModelAlias] = m.Evidence.Passed
	}
	if rates["cheap"] != 90 || rates["higher-overall"] != 98 || rates["rescue"] != 96 {
		t.Fatalf("wrong overall rates: %+v", rates)
	}
	rescue := map[string]float64{}
	for _, e := range report.Rescue {
		if e.From.ModelAlias == "cheap" {
			rescue[e.To.ModelAlias] = e.SuccessRate()
			if e.CorpusSHA256 != report.QualificationSHA256 {
				t.Fatal("unbound corpus")
			}
		}
	}
	if rescue["higher-overall"] != 0.8 || rescue["rescue"] != 1 {
		t.Fatalf("wrong conditional rates: %+v", rescue)
	}
	if !report.Verifier.Passed || report.Chain.Solved != 20 || report.Chain.TotalAttempts != 22 || report.Chain.TotalEstimatedCents != 4 || report.Chain.SuccessRate != 1 || report.TotalEstimatedCents != 304 {
		t.Fatalf("wrong chain: %+v", report)
	}
	if config.Models[1].Client.(*numericProvider).calls != 100 || config.Models[2].Client.(*numericProvider).calls != 102 {
		t.Fatal("higher-overall model used despite insufficient rescue rate")
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "Content") || strings.Contains(string(raw), "Messages") {
		t.Fatal("report contains source/output text")
	}
}

func TestCascadeEvaluationRejectsInvalidVerifierAndLeakedHoldoutBeforeDispatch(t *testing.T) {
	for _, kind := range []string{"bad-verifier", "overlap-id", "overlap-input", "budget", "only-negatives", "duplicate", "typed-tools"} {
		t.Run(kind, func(t *testing.T) {
			config := evaluationConfig()
			set := evaluationSet()
			options := evaluationOptions()
			switch kind {
			case "bad-verifier":
				config.Tasks[0].Verify = func(context.Context, ports.ChatRequest, *ports.ChatResponse) (bool, error) { return true, nil }
			case "overlap-id":
				set.Validation[0].ID = set.Qualification[0].ID
			case "overlap-input":
				set.Validation[0].Request = set.Qualification[0].Request
			case "budget":
				options.MaxEstimatedCents = 1
			case "only-negatives":
				set.VerifierCases = set.VerifierCases[1:]
			case "duplicate":
				set.Qualification[1].ID = set.Qualification[0].ID
			case "typed-tools":
				set.Validation[0].Request.Tools = []ports.ToolSpec{{Name: "write"}}
			}
			if _, err := newCascade(t, config).EvaluateCascade(context.Background(), cascadeKey, set, options); err == nil {
				t.Fatal("invalid evaluation accepted")
			}
			for _, m := range config.Models {
				if m.Client.(*numericProvider).calls > 0 {
					t.Fatal("invalid corpus or audit caused dispatch")
				}
			}
		})
	}
}

func TestCascadeRescueGateSkipsInsufficientResidualEvidence(t *testing.T) {
	config := cascadeConfig()
	policy := llmkit.RescuePolicy{MinSamples: 5, MinSuccessRate: 0.8}
	config.RescuePolicy = &policy
	for i := range config.Evidence {
		config.Evidence[i].CorpusSHA256 = strings.Repeat("a", 64)
	}
	from := config.Evidence[1]
	for _, pair := range []struct{ index, rescued int }{{3, 7}, {4, 14}} {
		to := config.Evidence[pair.index]
		config.RescueEvidence = append(config.RescueEvidence, llmkit.RescueEvidence{Task: cascadeKey, CorpusSHA256: from.CorpusSHA256, From: from.ModelKey(), To: to.ModelKey(), Failed: 14, Rescued: pair.rescued, EvaluatedAt: from.EvaluatedAt, ExpiresAt: from.ExpiresAt})
	}
	result, err := newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{})
	if err != nil || len(result.Attempts) != 2 || result.Attempts[1].ModelAlias != "strong" {
		t.Fatalf("ignored rescue evidence: %+v %v", result, err)
	}
	if len(config.Models[3].Client.(*fakeProviderClient).requests) != 0 {
		t.Fatal("insufficient rescue candidate called")
	}
	config = cascadeConfig()
	config.RescuePolicy = &policy
	result, err = newCascade(t, config).Execute(context.Background(), cascadeKey, "task", ports.ChatRequest{})
	if !errors.Is(err, ErrCascadeExhausted) || len(result.Attempts) != 1 {
		t.Fatalf("missing paired evidence not closed: %+v %v", result, err)
	}
}
