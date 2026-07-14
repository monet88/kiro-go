package proxy

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"kiro-go/logger"
)

// 工具定义体积压缩
//
// 当一次请求携带的工具定义（name + description + inputSchema）序列化后的总字节数
// 超过阈值时，对齐 kiro-rs 的 compress_tools_if_needed 做两步渐进压缩，避免把超大的
// tools 数组原样上送、被 Kiro 上游以 "Improperly formed request." 拒绝：
//  1. 递归简化每个工具的 inputSchema：保留结构与选参约束（type / required / enum /
//     $ref / anyOf|oneOf|allOf / properties / items），剥掉 description / examples /
//     default / title 等纯说明性字段（体积大头，对选参非必需）。
//  2. 若简化 schema 后仍超阈值，按超出比例截断每个工具的 description（UTF-8 安全，至少
//     保留 minToolDescChars 个字符，以免描述被砍到无法辨识工具用途）。
//
// 注意：本压缩是“总量兜底”，与既有的 per-tool 上限（maxToolDescLen）和非法字段清理
// （cleanSchema）互补，不替代它们——前两者在单个工具维度先行处理，本函数只在所有工具
// 加起来仍然过大时才介入。
//
// 与 upstream_error_hint.go 的 isImproperlyFormedRejection 互补：那边在被上游拒绝后
// 把 opaque 文案改写成可操作 hint；这里在发送前做预防、尽量不让请求被拒。

// defaultToolsSizeThreshold 是触发工具压缩的总字节阈值。20KB 对齐 kiro-rs 的
// TOOL_SIZE_THRESHOLD 经验值；Kiro 上游真实红线未公开，可经 env 按实测调整。
const defaultToolsSizeThreshold = 20 * 1024

// minToolDescChars 是 description 截断后至少保留的字符数（rune 计，UTF-8 安全），
// 保证压缩后描述仍可辨识工具用途。对齐 kiro-rs 的 MIN_DESCRIPTION_CHARS。
const minToolDescChars = 50

// resolveToolsSizeThreshold 读取 env 覆盖（KIRO_TOOLS_COMPRESS_THRESHOLD_BYTES）。
// 非法值回落默认；0 表示禁用压缩（退回原样上送，由上游与既有 per-tool 上限兜底）。
func resolveToolsSizeThreshold() int {
	raw := strings.TrimSpace(os.Getenv("KIRO_TOOLS_COMPRESS_THRESHOLD_BYTES"))
	if raw == "" {
		return defaultToolsSizeThreshold
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultToolsSizeThreshold
	}
	return n
}

// toolsSizeThresholdCache memoizes the resolved threshold so the per-request hot
// path (compressToolsIfNeeded on every tool-bearing request) does not call
// os.Getenv each time — Go's environment lookup takes a global lock and would
// serialize under high concurrency. Sentinel -1 means "not yet resolved";
// concurrent resolves are harmless since they compute the same value.
var toolsSizeThresholdCache atomic.Int64

func init() {
	toolsSizeThresholdCache.Store(-1)
}

// cachedToolsSizeThreshold returns the memoized threshold, resolving the env at
// most once. Tests that change the env var call resetToolsSizeThresholdCache so
// the next lookup re-reads it.
func cachedToolsSizeThreshold() int {
	if v := toolsSizeThresholdCache.Load(); v >= 0 {
		return int(v)
	}
	resolved := resolveToolsSizeThreshold()
	toolsSizeThresholdCache.Store(int64(resolved))
	return resolved
}

// resetToolsSizeThresholdCache clears the memoized threshold so the next lookup
// re-reads KIRO_TOOLS_COMPRESS_THRESHOLD_BYTES. Intended for tests that mutate
// the env var via t.Setenv.
func resetToolsSizeThresholdCache() {
	toolsSizeThresholdCache.Store(-1)
}

// compressToolsIfNeeded resolves the operator threshold (memoized) and logs when
// a compression pass actually shrinks the tool list. Algorithm lives in compressTools.
func compressToolsIfNeeded(tools []KiroToolWrapper) []KiroToolWrapper {
	threshold := cachedToolsSizeThreshold()
	if threshold <= 0 || len(tools) == 0 {
		return tools
	}
	before := estimateToolsBytes(tools)
	out := compressTools(tools, threshold)
	after := estimateToolsBytes(out)
	if before > threshold && after < before {
		logger.Infof("[ToolCompress] %d tools compressed: %d -> %d bytes (threshold %d)",
			len(tools), before, after, threshold)
	}
	return out
}

// compressTools runs the pure progressive compression algorithm against an
// explicit threshold. threshold<=0 or empty tools is a no-op. Callers that need
// env resolution use compressToolsIfNeeded.
func compressTools(tools []KiroToolWrapper, threshold int) []KiroToolWrapper {
	if threshold <= 0 || len(tools) == 0 {
		return tools
	}

	total := estimateToolsBytes(tools)
	if total <= threshold {
		return tools
	}

	// Step 1: recursively simplify each tool's inputSchema.
	for i := range tools {
		tools[i].ToolSpecification.InputSchema.JSON = simplifyToolSchema(tools[i].ToolSpecification.InputSchema.JSON)
	}

	afterSchema := estimateToolsBytes(tools)
	if afterSchema <= threshold {
		return tools
	}

	// Step 2: truncate descriptions by byte ratio (UTF-8 safe, minToolDescChars floor).
	ratio := float64(threshold) / float64(afterSchema)
	for i := range tools {
		desc := tools[i].ToolSpecification.Description
		tools[i].ToolSpecification.Description = truncateDescByRatio(desc, ratio)
	}
	return tools
}

// estimateToolsBytes 估算工具列表序列化后的总字节数。直接对整个 slice 做 json.Marshal
// 取长度，口径与上送前 json.Marshal(payload) 内 tools 部分一致——把 toolSpecification /
// name / description / inputSchema 等 JSON key、引号、括号、逗号等结构开销也算进去，
// 避免只累加字段长度导致的系统性低估（低估会让临界请求“该压不压”仍被上游拒）。
// Marshal 失败（理论上不会，KiroToolWrapper 全是可序列化字段）时回退到字段长度累加。
func estimateToolsBytes(tools []KiroToolWrapper) int {
	if raw, err := json.Marshal(tools); err == nil {
		return len(raw)
	}
	total := 0
	for i := range tools {
		spec := tools[i].ToolSpecification
		total += len(spec.Name) + len(spec.Description)
		if raw, err := json.Marshal(spec.InputSchema.JSON); err == nil {
			total += len(raw)
		}
	}
	return total
}

// truncateDescByRatio 按 ratio（字节比例）截断 desc，UTF-8 安全，至少保留
// minToolDescChars 个字符。ratio>=1 时不截断。
//
// ratio = threshold / afterSchemaBytes 是按字节算出的，故 target 也按字节算（而非乘
// rune 数），口径自洽——否则对 CJK 这类多字节文本会把字节比误当字符比，导致过度截断。
// 取到目标字节数后回退到不超过它的最近 UTF-8 字符边界，保证不切坏多字节字符。
//
// 为避免对大描述反复分配 []rune，用 range 一次遍历定位第 minToolDescChars 个 rune 的
// 字节下标（minBytes），同时得到总 rune 数（runeCount）。
func truncateDescByRatio(desc string, ratio float64) string {
	if ratio >= 1 {
		return desc
	}

	// 一次遍历：定位第 minToolDescChars 个 rune 的字节下标，并统计总 rune 数。
	runeCount := 0
	minBytes := len(desc)
	for byteIdx := range desc {
		if runeCount == minToolDescChars {
			minBytes = byteIdx
		}
		runeCount++
	}
	// rune 数不超过保底值 → 不截断。
	if runeCount <= minToolDescChars {
		return desc
	}

	targetBytes := int(float64(len(desc)) * ratio)
	// 保底：至少保留 minToolDescChars 个字符对应的字节数。
	if targetBytes < minBytes {
		targetBytes = minBytes
	}
	if targetBytes >= len(desc) {
		return desc
	}

	// 回退到 <= targetBytes 的最近字符边界（避免切坏多字节 rune）。
	cut := targetBytes
	for cut > 0 && !utf8.RuneStart(desc[cut]) {
		cut--
	}
	return desc[:cut]
}

// simplifyToolSchema 递归简化 JSON Schema：保留结构与约束骨架，剥掉纯说明性字段。
//   - 保留：type、required、enum、$ref、anyOf/oneOf/allOf、properties、items（结构与
//     选参约束相关，去掉会改变上游/模型对参数的理解）。
//   - 移除：description、examples、default、title、$comment 等纯说明性字段（体积大头，
//     对模型选参非必需）。
//
// 处理 map[string]interface{}（对象 schema）与 []interface{}（如 tuple 校验里
// items 为 schema 数组）两种形态；其他标量形态原样返回。注意：本函数返回新值，
// 不修改入参。
func simplifyToolSchema(schema interface{}) interface{} {
	switch val := schema.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})

		// 保留顶层结构 / 约束字段（剔除 additionalProperties——cleanSchema 已要求移除它）。
		// $ref / anyOf / oneOf / allOf 在缺 type 时是该节点唯一的语义来源，必须保留，否则
		// 节点会塌成 {} 让模型与上游都无法理解。
		copySchemaKeptKeys(val, result)

		// properties：递归简化每个属性。
		if props, ok := val["properties"].(map[string]interface{}); ok {
			simplified := make(map[string]interface{}, len(props))
			for name, prop := range props {
				simplified[name] = simplifyToolSchema(prop)
			}
			result["properties"] = simplified
		}

		// items（数组元素 schema）：递归简化（可能是单个 schema 或 schema 数组）。
		if items, exists := val["items"]; exists {
			result["items"] = simplifyToolSchema(items)
		}

		return result
	case []interface{}:
		// tuple 校验（items 为 schema 数组）等情形：逐元素递归简化，避免整段原样透传
		// 导致嵌套说明性字段没被剥掉。
		simplified := make([]interface{}, len(val))
		for i, item := range val {
			simplified[i] = simplifyToolSchema(item)
		}
		return simplified
	default:
		return schema
	}
}

// schemaKeptKeys 是简化时保留的非递归字段：结构（type/required）、选参约束（enum）、
// 组合/引用（$ref/anyOf/oneOf/allOf）、以及 $schema。此外保留常用校验约束
// （pattern/长度/数值范围/数组约束等）——剥掉它们会让模型生成违反原 schema 的参数，
// 导致工具执行失败。description/examples/default/title 等纯说明性字段不在此列，会被剥除。
var schemaKeptKeys = []string{
	"$schema", "type", "required", "enum", "$ref", "anyOf", "oneOf", "allOf",
	"pattern", "minLength", "maxLength", "format",
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minItems", "maxItems", "uniqueItems",
}

// copySchemaKeptKeys 把 src 中 schemaKeptKeys 列出的字段拷到 dst。对 anyOf/oneOf/allOf
// 这类「子 schema 数组」会递归简化其中每个元素。
func copySchemaKeptKeys(src, dst map[string]interface{}) {
	for _, key := range schemaKeptKeys {
		v, exists := src[key]
		if !exists {
			continue
		}
		switch key {
		case "anyOf", "oneOf", "allOf":
			if arr, ok := v.([]interface{}); ok {
				simplifiedArr := make([]interface{}, len(arr))
				for i, sub := range arr {
					simplifiedArr[i] = simplifyToolSchema(sub)
				}
				dst[key] = simplifiedArr
				continue
			}
			dst[key] = v
		default:
			dst[key] = v
		}
	}
}
