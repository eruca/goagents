package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eruca/goagents/goagent/prompt"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var ErrOutputInvalid = errors.New("output invalid")

// OutputFormat describes the final answer shape the host wants the model to
// produce. JSONSchema is both prompt guidance and a local validation contract.
type OutputFormat struct {
	Name         string
	Description  string
	Instructions string
	JSONSchema   json.RawMessage
}

// OutputValidationRequest is passed to host validators after the model returns
// a final answer and before the answer is saved to memory.
type OutputValidationRequest struct {
	RunID    RunID
	Content  string
	Format   OutputFormat
	Metadata map[string]any
}

// OutputValidationResult lets custom validators attach parsed output or small
// validation metadata to the final RunResult.
type OutputValidationResult struct {
	Structured json.RawMessage
	Metadata   map[string]any
}

// OutputValidator is a host-owned final-answer guardrail. It should validate
// output shape or safety, not call tools or mutate external state.
type OutputValidator interface {
	ValidateOutput(context.Context, OutputValidationRequest) (*OutputValidationResult, error)
}

type OutputValidatorFunc func(context.Context, OutputValidationRequest) (*OutputValidationResult, error)

func (f OutputValidatorFunc) ValidateOutput(ctx context.Context, req OutputValidationRequest) (*OutputValidationResult, error) {
	return f(ctx, req)
}

// OutputFormatStage adds final-output instructions before the prompt is
// compiled. Validation still happens later in FinalizeStage.
type OutputFormatStage struct {
	Format OutputFormat
}

func (s OutputFormatStage) Name() string {
	return "output_format"
}

func (s OutputFormatStage) Run(ctx context.Context, state *RunState) (StageResult, error) {
	if s.Format.IsZero() || state.Metadata[outputFormatLoadedKey] == true {
		return StageContinue, nil
	}
	content := s.Format.promptContent()
	if content != "" {
		state.PromptBlocks = append(state.PromptBlocks, prompt.Block{
			Name:     "agentcore.output_format",
			Mode:     prompt.ModeDynamic,
			Priority: 1000,
			Content:  content,
		})
	}
	state.Metadata[outputFormatLoadedKey] = true
	return StageContinue, nil
}

func (f OutputFormat) IsZero() bool {
	return strings.TrimSpace(f.Name) == "" &&
		strings.TrimSpace(f.Description) == "" &&
		strings.TrimSpace(f.Instructions) == "" &&
		len(f.JSONSchema) == 0
}

func (f OutputFormat) promptContent() string {
	parts := []string{"Output format:"}
	if name := strings.TrimSpace(f.Name); name != "" {
		parts = append(parts, "name: "+name)
	}
	if description := strings.TrimSpace(f.Description); description != "" {
		parts = append(parts, "description: "+description)
	}
	if instructions := strings.TrimSpace(f.Instructions); instructions != "" {
		parts = append(parts, instructions)
	}
	if len(f.JSONSchema) > 0 {
		parts = append(parts, "Return the final answer as JSON matching this JSON Schema. Do not wrap the JSON in Markdown fences.")
		parts = append(parts, string(f.JSONSchema))
	}
	return strings.Join(parts, "\n")
}

func validateFinalOutput(ctx context.Context, state *RunState, content string, format OutputFormat, validator OutputValidator) (json.RawMessage, map[string]any, error) {
	var structured json.RawMessage
	var metadata map[string]any
	// Schema validation runs before the custom validator so host logic can rely
	// on the declared JSON shape when both contracts are configured.
	if len(format.JSONSchema) > 0 {
		result, err := validateJSONOutput(format, content)
		if err != nil {
			return nil, nil, err
		}
		structured = result.Structured
		metadata = mergeOutputMetadata(metadata, result.Metadata)
	}
	if validator != nil {
		result, err := validator.ValidateOutput(ctx, OutputValidationRequest{
			RunID:    state.RunID,
			Content:  content,
			Format:   cloneOutputFormat(format),
			Metadata: cloneMetadata(state.Metadata),
		})
		if err != nil {
			return nil, nil, outputInvalidError(err)
		}
		if result != nil {
			if len(result.Structured) > 0 {
				structured = append(json.RawMessage(nil), result.Structured...)
			}
			metadata = mergeOutputMetadata(metadata, result.Metadata)
		}
	}
	return structured, metadata, nil
}

func validateJSONOutput(format OutputFormat, content string) (*OutputValidationResult, error) {
	raw := json.RawMessage(strings.TrimSpace(content))
	if len(raw) == 0 {
		return nil, outputInvalidError(fmt.Errorf("empty final output"))
	}
	if !json.Valid(raw) {
		return nil, outputInvalidError(fmt.Errorf("final output is not valid JSON"))
	}
	if err := validateOutputJSONSchema(format.JSONSchema, raw); err != nil {
		return nil, outputInvalidError(err)
	}
	return &OutputValidationResult{
		Structured: append(json.RawMessage(nil), raw...),
		Metadata: map[string]any{
			"output_format": format.Name,
			"schema":        "json",
		},
	}, nil
}

func validateOutputJSONSchema(schema json.RawMessage, input json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	if err != nil {
		return fmt.Errorf("invalid output JSON schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("output.schema.json", doc); err != nil {
		return fmt.Errorf("invalid output JSON schema: %w", err)
	}
	compiled, err := compiler.Compile("output.schema.json")
	if err != nil {
		return fmt.Errorf("invalid output JSON schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(input)))
	if err != nil {
		return fmt.Errorf("invalid output JSON: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		return fmt.Errorf("output failed JSON schema validation: %w", err)
	}
	return nil
}

func outputInvalidError(err error) error {
	if errors.Is(err, ErrOutputInvalid) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrOutputInvalid, err)
}

func cloneOutputFormat(format OutputFormat) OutputFormat {
	format.JSONSchema = append(json.RawMessage(nil), format.JSONSchema...)
	return format
}

func mergeOutputMetadata(base map[string]any, next map[string]any) map[string]any {
	if len(next) == 0 {
		return base
	}
	if base == nil {
		base = make(map[string]any, len(next))
	}
	for key, value := range next {
		base[key] = value
	}
	return base
}

const outputFormatLoadedKey = "agentcore.output_format.loaded"
