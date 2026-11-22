package handoff

import (
	"encoding/json"
	"fmt"

	"github.com/eruca/goagents/goagent/ports"
)

// Tool spec styles.
const (
	ToolSpecCompact = "compact" // one sentence and the key parameters
	ToolSpecFull    = "full"    // gocms' own descriptions and schemas
)

// compactTool is the short form of one gocms tool for this use: the
// overview is already in the prompt, so the model only needs to know what
// each tool adds and the few parameters worth setting.
type compactTool struct {
	description string
	params      []string // kept properties; the rest use gocms defaults
}

// compactTools covers ModelTools. A tool missing here keeps its full spec.
// Descriptions are model-facing, so they are Chinese like gocms' own.
var compactTools = map[string]compactTool{
	"labs": {
		"检验：默认近24小时每项一行（首末、最高最低）。items 写项目名或简称（血钾、K、Lac），hours 改时长，mode=raw 看逐条。",
		[]string{"items", "hours", "mode", "abnormal_only"},
	},
	"vitals": {
		"护理记录单的体征、呼吸机参数、意识瞳孔：默认近24小时范围和末次。items 写项目名（心率、MAP、FiO2），hours 改时长，mode=raw 看逐条。",
		[]string{"items", "hours", "mode", "abnormal_only"},
	},
	"drugs": {
		"用药起止、剂量调整、是否在用：默认升压药、抗生素、镇静镇痛、抗凝。drug 按药名找，categories 按分类，active_only 只要在用的。",
		[]string{"drug", "categories", "active_only", "hours"},
	},
	"io": {
		"出入量（按统计日）：默认今日至今和昨日整天。day 指定统计日（YYYY-MM-DD），days 往前几天，detail=shift 分班次。",
		[]string{"day", "days", "detail"},
	},
	"scores": {
		"评分和量表（SOFA、APACHE II、GCS、RASS 等）的总分、各项得分和变化。instruments 写量表名，hours 改时长。",
		[]string{"instruments", "hours", "latest"},
	},
	"alerts": {
		"未处理的提醒、被排除的数据、工作流标记的具体内容（概览只给条数）。",
		[]string{"include", "status"},
	},
	"timeline": {
		"按时间排的事件：转床、上机撤机、重点药开停调整、危急值、微生物、手术操作、评分变化。hours 改时长，kinds 选种类。",
		[]string{"hours", "kinds", "group"},
	},
	"expand": {
		"展开引用（原样取自工具返回）看那条数据的全部细节；context=true 加前后12小时同项目的值。",
		[]string{"refs", "context"},
	},
}

// compactSpec replaces a tool's description and keeps only its key
// parameters, without their descriptions. Types, enums and bounds stay, so
// the model still sends valid values; gocms validates every call anyway.
func compactSpec(spec ports.ToolSpec) (ports.ToolSpec, error) {
	c, ok := compactTools[spec.Name]
	if !ok || len(spec.Schema.JSONSchema) == 0 {
		return spec, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(spec.Schema.JSONSchema, &schema); err != nil {
		return spec, fmt.Errorf("tool %s: input schema: %w", spec.Name, err)
	}
	keep := map[string]bool{}
	for _, name := range c.params {
		keep[name] = true
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for name, prop := range props {
			if !keep[name] {
				delete(props, name)
				continue
			}
			if m, ok := prop.(map[string]any); ok {
				delete(m, "description")
			}
		}
	}
	if required, ok := schema["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, name := range required {
			if s, ok := name.(string); ok && keep[s] {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = kept
		}
	}
	delete(schema, "description")
	raw, err := json.Marshal(schema)
	if err != nil {
		return spec, err
	}
	spec.Description = c.description
	spec.Schema = ports.ToolSchema{JSONSchema: raw}
	return spec, nil
}
