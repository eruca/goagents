package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/eruca/goagents/goagent/agentcore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Point is one handoff fact: a sentence or two and the references behind it.
type Point struct {
	Text string   `json:"text"`
	Refs []string `json:"refs"`
}

// Summary is one patient's structured answer from the model.
type Summary struct {
	Patient   string   `json:"patient"`
	Points    []Point  `json:"points"`
	Watch     []string `json:"watch"`
	Uncertain []string `json:"uncertain"`
}

// OutputSchema is the per-patient answer contract (gocms design §10.2).
var OutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["patient", "points", "watch", "uncertain"],
  "properties": {
    "patient": {"type": "string", "minLength": 1, "maxLength": 24},
    "points": {
      "type": "array", "minItems": 3, "maxItems": 6,
      "items": {
        "type": "object",
        "required": ["text", "refs"],
        "properties": {
          "text": {"type": "string", "minLength": 1, "maxLength": 300},
          "refs": {"type": "array", "maxItems": 12, "items": {"type": "string", "maxLength": 24}}
        },
        "additionalProperties": false
      }
    },
    "watch": {"type": "array", "maxItems": 3, "items": {"type": "string", "minLength": 1, "maxLength": 120}},
    "uncertain": {"type": "array", "maxItems": 5, "items": {"type": "string", "minLength": 1, "maxLength": 200}}
  },
  "additionalProperties": false
}`)

// OutputFormat is the agentcore output contract. The schema is written into
// the instructions rather than OutputFormat.JSONSchema so that validation goes
// through validateOutput, which tolerates a Markdown fence around the JSON;
// local models add one often.
func OutputFormat() agentcore.OutputFormat {
	return agentcore.OutputFormat{
		Name:        "icu_handoff_patient",
		Description: "一位病人的交班要点",
		Instructions: "最终答复只输出一个 JSON 对象，符合下面的 JSON Schema，不要加任何其他文字：\n" +
			string(OutputSchema),
	}
}

var (
	compiledOnce   sync.Once
	compiledSchema *jsonschema.Schema
	compileErr     error
)

func outputSchema() (*jsonschema.Schema, error) {
	compiledOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(OutputSchema)))
		if err != nil {
			compileErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("handoff.schema.json", doc); err != nil {
			compileErr = err
			return
		}
		compiledSchema, compileErr = compiler.Compile("handoff.schema.json")
	})
	return compiledSchema, compileErr
}

// errNoJSON is returned when the answer holds no JSON object at all.
var errNoJSON = errors.New("final answer contains no JSON object")

// extractJSON returns the JSON object in content, dropping a Markdown fence
// or text around it.
func extractJSON(content string) (string, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		content = strings.TrimPrefix(content, "```json")
		content = strings.TrimPrefix(content, "```")
		if end := strings.LastIndex(content, "```"); end >= 0 {
			content = content[:end]
		}
		content = strings.TrimSpace(content)
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return "", errNoJSON
	}
	return content[start : end+1], nil
}

// ValidateOutput is the agentcore output validator: it extracts the JSON
// object and checks it against OutputSchema. Errors are wrapped by agentcore
// as ErrOutputInvalid.
func ValidateOutput(_ context.Context, req agentcore.OutputValidationRequest) (*agentcore.OutputValidationResult, error) {
	raw, err := extractJSON(req.Content)
	if err != nil {
		return nil, err
	}
	schema, err := outputSchema()
	if err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("final answer is not valid JSON: %w", err)
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("final answer does not match the schema: %w", err)
	}
	return &agentcore.OutputValidationResult{
		Structured: json.RawMessage(raw),
		Metadata:   map[string]any{"output_format": "icu_handoff_patient"},
	}, nil
}
