// Package api 实现脉冲配对服务的 HTTP 层：严格的请求解析、按输入位置
// 汇总的字段级校验错误，以及调用全局动态规划产出可复算结果。
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"

	"github.com/gin-gonic/gin"

	"packageline/internal/match"
)

// 参数上界（delay/window/penalty 均为闭区间 [0, maxParam]）。
const maxParam = 1_000_000

// 请求体大小上界（字节）。
const maxBodyBytes int64 = 1 << 20

// intLiteral 匹配 JSON 整数写法：允许负号与 0，不允许小数点/指数/前导零。
var intLiteral = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// fieldError 是一条定位到输入位置的字段错误。Position 为该值 token
// 结束处相对请求体起始的字节偏移，用于按输入位置稳定排序。
type fieldError struct {
	Position int64  `json:"position"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// rawField 保存一个已解析字段的字面值与偏移。
type rawField struct {
	literals    []string // 数组字段为逐元素字面值，标量字段取第一个
	elemOffsets []int64  // 数组字段逐元素的偏移
	offset      int64    // 字段值（数组为首元素或标量本身）的偏移
	present     bool
	isArray     bool
	wrongType   bool // 数组字段收到非数组值，已上报 not_an_array
}

type parsedRequest struct {
	upstream   rawField
	downstream rawField
	delay      rawField
	window     rawField
	penalty    rawField
}

// Handler 注册并提供唯一的业务接口。
type Handler struct{}

// NewHandler 构造 Handler。
func NewHandler() *Handler { return &Handler{} }

// Register 在给定引擎上注册路由。
func (h *Handler) Register(r *gin.Engine) {
	r.POST("/match", h.match)
}

func (h *Handler) match(c *gin.Context) {
	// 多读 1 字节用于识别超限。
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []fieldError{{
			Field:   "_body",
			Code:    "unreadable_body",
			Message: "request body could not be read",
		}}})
		return
	}
	if int64(len(body)) > maxBodyBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"errors": []fieldError{{
			Field:   "_body",
			Code:    "body_too_large",
			Message: "request body exceeds 1 MiB",
		}}})
		return
	}

	req, parseErrs, fatal := parseRequest(body)
	if fatal {
		// JSON 结构已损坏，无法继续定位后续字段：返回已收集的错误。
		sortErrors(parseErrs)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": parseErrs})
		return
	}
	// 解析期语义错误（not_an_array/unknown_field/duplicate_field）与
	// 逐字段语义校验错误合并，整批返回；只要存在任一错误即不产出局部结果。
	errs := append(parseErrs, validate(req)...)
	if len(errs) > 0 {
		sortErrors(errs)
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": errs})
		return
	}

	upstream := mustInts(req.upstream)
	downstream := mustInts(req.downstream)
	delay := mustOne(req.delay)
	window := mustOne(req.window)
	penalty := mustOne(req.penalty)

	result := match.Solve(upstream, downstream, delay, window, penalty)
	c.JSON(http.StatusOK, result)
}

func mustInts(f rawField) []int64 {
	out := make([]int64, len(f.literals))
	for i, l := range f.literals {
		v, _ := strconv.ParseInt(l, 10, 64)
		out[i] = v
	}
	return out
}

func mustOne(f rawField) int64 {
	v, _ := strconv.ParseInt(f.literals[0], 10, 64)
	return v
}

// sortErrors 按输入位置升序；位置相同按字段名、错误码稳定排序。
func sortErrors(errs []fieldError) {
	sort.SliceStable(errs, func(a, b int) bool {
		if errs[a].Position != errs[b].Position {
			return errs[a].Position < errs[b].Position
		}
		if errs[a].Field != errs[b].Field {
			return errs[a].Field < errs[b].Field
		}
		return errs[a].Code < errs[b].Code
	})
}

// validate 在结构解析成功后执行全部语义校验，收集所有错误（不在首个
// 错误处停止），保证“整批返回全部错误”。
func validate(req parsedRequest) []fieldError {
	var errs []fieldError

	errs = validateTimestamps(req.upstream, "upstream", errs)
	errs = validateTimestamps(req.downstream, "downstream", errs)
	errs = validateParam(req.delay, "delay", errs)
	errs = validateParam(req.window, "window", errs)
	errs = validateParam(req.penalty, "penalty", errs)

	return errs
}

func validateTimestamps(f rawField, name string, errs []fieldError) []fieldError {
	if !f.present {
		return append(errs, fieldError{
			Position: f.offset, Field: name, Code: "required",
			Message: fmt.Sprintf("%s is required", name),
		})
	}
	if f.wrongType {
		return errs // not_an_array 已在解析阶段上报，避免级联误报
	}

	values := make([]int64, len(f.literals))
	valid := make([]bool, len(f.literals))
	for i, lit := range f.literals {
		loc := fmt.Sprintf("%s[%d]", name, i)
		v, isInt := parseInteger(lit)
		if !isInt {
			errs = append(errs, fieldError{
				Position: elementOffset(f, i), Field: loc, Code: "not_integer",
				Message: fmt.Sprintf("%s must be an integer", loc),
			})
			continue
		}
		if v < 0 {
			errs = append(errs, fieldError{
				Position: elementOffset(f, i), Field: loc, Code: "negative_integer",
				Message: fmt.Sprintf("%s must be non-negative", loc),
			})
			continue
		}
		values[i], valid[i] = v, true
	}

	for i := 1; i < len(f.literals); i++ {
		// 仅当相邻两元素均为合法整数时检查严格递增，避免级联误报。
		if valid[i-1] && valid[i] && values[i] <= values[i-1] {
			loc := fmt.Sprintf("%s[%d]", name, i)
			errs = append(errs, fieldError{
				Position: elementOffset(f, i), Field: loc, Code: "not_strictly_increasing",
				Message: fmt.Sprintf("%s must be strictly greater than %s[%d]", loc, name, i-1),
			})
		}
	}
	return errs
}

func validateParam(f rawField, name string, errs []fieldError) []fieldError {
	if !f.present {
		return append(errs, fieldError{
			Position: f.offset, Field: name, Code: "required",
			Message: fmt.Sprintf("%s is required", name),
		})
	}
	v, isInt := parseInteger(f.literals[0])
	if !isInt {
		return append(errs, fieldError{
			Position: f.offset, Field: name, Code: "not_integer",
			Message: fmt.Sprintf("%s must be an integer", name),
		})
	}
	if v < 0 || v > maxParam {
		return append(errs, fieldError{
			Position: f.offset, Field: name, Code: "out_of_range",
			Message: fmt.Sprintf("%s must be between 0 and %d", name, maxParam),
		})
	}
	return errs
}

// parseInteger 接受 JSON 整数字面量（可负、可由 int64 表示），
// 拒绝浮点、指数、前导零、字符串、布尔、null 等写法。
func parseInteger(lit string) (int64, bool) {
	if !intLiteral.MatchString(lit) {
		return 0, false
	}
	v, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func elementOffset(f rawField, i int) int64 {
	// 字面值逐元素来自顺序 token 流；解析阶段已为每个元素记录偏移，
	// 这里直接复用（见 rawField.elemOffsets）。
	if i < len(f.elemOffsets) {
		return f.elemOffsets[i]
	}
	return f.offset
}

// parseRequest 使用 json.Decoder 的 token 流做位置感知解析。
// 返回解析出的字段、解析期语义错误（可与后续校验错误合并）以及 fatal
// 标志；fatal 为真表示 JSON 结构已损坏，无法再安全解析后续字段。
func parseRequest(body []byte) (req parsedRequest, errs []fieldError, fatal bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return req, []fieldError{{Position: 0, Field: "_body", Code: "invalid_json",
			Message: "request body must be a JSON object"}}, true
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return req, []fieldError{{Position: dec.InputOffset(), Field: "_body", Code: "invalid_json",
			Message: "malformed JSON: " + err.Error()}}, true
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return req, []fieldError{{Position: dec.InputOffset(), Field: "_body", Code: "invalid_json",
			Message: "request body must be a JSON object"}}, true
	}

	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return req, append(errs, fieldError{Position: dec.InputOffset(), Field: "_body",
				Code: "invalid_json", Message: "malformed JSON: " + err.Error()}), true
		}
		key, ok := keyTok.(string)
		if !ok {
			return req, []fieldError{{Position: dec.InputOffset(), Field: "_body", Code: "invalid_json",
				Message: "object key must be a string"}}, true
		}
		if seen[key] {
			errs = append(errs, fieldError{Position: dec.InputOffset(), Field: key,
				Code: "duplicate_field", Message: fmt.Sprintf("field %q appears more than once", key)})
			// 保留首个出现的字段值：跳过重复出现的整个值。
			if err := skipValue(dec); err != nil {
				return req, append(errs, fieldError{Position: dec.InputOffset(), Field: "_body",
					Code: "invalid_json", Message: "malformed JSON"}), true
			}
			continue
		}
		seen[key] = true

		var f *rawField
		var isArray bool
		switch key {
		case "upstream":
			f, isArray = &req.upstream, true
		case "downstream":
			f, isArray = &req.downstream, true
		case "delay":
			f = &req.delay
		case "window":
			f = &req.window
		case "penalty":
			f = &req.penalty
		default:
			start := dec.InputOffset()
			if err := skipValue(dec); err != nil {
				return req, append(errs, fieldError{Position: dec.InputOffset(), Field: "_body",
					Code: "invalid_json", Message: "malformed JSON"}), true
			}
			errs = append(errs, fieldError{Position: start, Field: key, Code: "unknown_field",
				Message: fmt.Sprintf("unknown field %q", key)})
			continue
		}

		sem, fErr := readField(dec, f, key, isArray)
		errs = append(errs, sem...)
		if fErr != nil {
			return req, append(errs, fErr...), true
		}
	}

	// 消费闭花括号。
	if _, err := dec.Token(); err != nil {
		return req, append(errs, fieldError{Position: dec.InputOffset(), Field: "_body",
			Code: "invalid_json", Message: "malformed JSON: " + err.Error()}), true
	}
	// 拒掉对象后的尾随内容。
	if t, err := dec.Token(); err != io.EOF {
		_ = t
		return req, append(errs, fieldError{Position: dec.InputOffset(), Field: "_body",
			Code: "invalid_json", Message: "trailing data after JSON object"}), true
	}

	return req, errs, false
}

// readField 读取下一个值到 f。返回值分两类：
//   - sem：字段级语义错误（如数组位给了标量、元素非整数），可安全继续
//     解析后续字段，用于“整批收集全部错误”；
//   - fatal：JSON 结构已被破坏的语法错误，必须立即中止。
func readField(dec *json.Decoder, f *rawField, key string, isArray bool) (sem, fatal []fieldError) {
	f.present = true

	if !isArray {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return nil, malformed(dec, err)
		}
		f.offset = start
		if d, ok := tok.(json.Delim); ok {
			if err := skipComposite(dec, d); err != nil {
				return nil, malformed(dec, err)
			}
			f.literals = []string{""} // 占位，触发 not_integer
			return nil, nil
		}
		if lit, ok := scalarLiteral(tok); ok {
			f.literals = []string{lit}
		} else {
			f.literals = []string{""} // null/bool 等：非整数
		}
		return nil, nil
	}

	// 数组字段。
	tok, err := dec.Token()
	if err != nil {
		return nil, malformed(dec, err)
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		// 标量出现在数组字段位置：语义错误，但该标量已被消费，可继续。
		f.offset = dec.InputOffset()
		f.isArray = false
		f.wrongType = true
		if lit, isLit := scalarLiteral(tok); isLit {
			f.literals = []string{lit}
		} else {
			f.literals = []string{""}
		}
		return []fieldError{{Position: f.offset, Field: key, Code: "not_an_array",
			Message: fmt.Sprintf("%s must be an array of integers", key)}}, nil
	}
	f.isArray = true

	idx := 0
	for dec.More() {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return nil, malformed(dec, err)
		}
		if idx == 0 {
			f.offset = start
		}
		if d, ok := tok.(json.Delim); ok {
			if err := skipComposite(dec, d); err != nil {
				return nil, malformed(dec, err)
			}
			f.literals = append(f.literals, "")
			f.elemOffsets = append(f.elemOffsets, start)
		} else if lit, ok := scalarLiteral(tok); ok {
			f.literals = append(f.literals, lit)
			f.elemOffsets = append(f.elemOffsets, start)
		} else {
			f.literals = append(f.literals, "")
			f.elemOffsets = append(f.elemOffsets, start)
		}
		idx++
	}
	// 消费 ']'。
	if _, err := dec.Token(); err != nil {
		return nil, malformed(dec, err)
	}
	return nil, nil
}

func malformed(dec *json.Decoder, err error) []fieldError {
	return []fieldError{{Position: dec.InputOffset(), Field: "_body",
		Code: "invalid_json", Message: "malformed JSON: " + err.Error()}}
}

func scalarLiteral(tok any) (string, bool) {
	switch v := tok.(type) {
	case json.Number:
		return string(v), true
	case string:
		// 字符串即使内容是数字也算非整数；返回带引号标记使其判为非法。
		return `"` + v + `"`, false
	default:
		return "", false // nil(null)、bool
	}
}

// skipValue 跳过任意一个即将读取的值。
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok {
		return skipComposite(dec, d)
	}
	return nil
}

// skipComposite 在已读到开分隔符 d 的前提下，消费到配对的闭分隔符。
func skipComposite(dec *json.Decoder, d json.Delim) error {
	var want json.Delim
	switch d {
	case '{':
		want = '}'
	case '[':
		want = ']'
	default:
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := tok.(json.Delim); ok {
			if dd == d {
				depth++
			} else if dd == want {
				depth--
			}
		}
	}
	return nil
}
