package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func newRouter() *gin.Engine {
	r := gin.New()
	NewHandler().Register(r)
	return r
}

func postJSON(t *testing.T, r *gin.Engine, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/match", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v body=%q", err, w.Body.String())
		}
	}
	return w.Code, out
}

func TestValidBasic(t *testing.T) {
	r := newRouter()
	code, out := postJSON(t, r, `{"upstream":[0,50],"downstream":[8,63],"delay":10,"window":5,"penalty":100}`)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%v", code, out)
	}
	if out["cost"].(float64) != 5 {
		t.Fatalf("cost = %v", out["cost"])
	}
	pairs := out["pairs"].([]any)
	if len(pairs) != 2 {
		t.Fatalf("pairs = %v", pairs)
	}
	// 非固定响应：换一组输入结果应不同。
	_, out2 := postJSON(t, r, `{"upstream":[0],"downstream":[],"delay":0,"window":0,"penalty":7}`)
	if out2["cost"].(float64) != 7 || len(out2["pairs"].([]any)) != 0 {
		t.Fatalf("second response looks fixed: %v", out2)
	}
}

func TestEmptyArrays(t *testing.T) {
	r := newRouter()
	code, out := postJSON(t, r, `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":1}`)
	if code != http.StatusOK {
		t.Fatalf("status=%d %v", code, out)
	}
	// 空数组必须是 [] 而非 null。
	raw, _ := json.Marshal(out["pairs"])
	if string(raw) != "[]" {
		t.Fatalf("pairs should be [], got %s", raw)
	}
	raw, _ = json.Marshal(out["unmatched_upstream"])
	if string(raw) != "[]" {
		t.Fatalf("unmatched_upstream should be [], got %s", raw)
	}
}

func TestWindowBoundaryInclusive(t *testing.T) {
	r := newRouter()
	// 残差恰为 +3，window=3 边界可配。
	code, out := postJSON(t, r, `{"upstream":[0,100],"downstream":[13,113],"delay":10,"window":3,"penalty":100}`)
	if code != 200 || out["cost"].(float64) != 6 || len(out["pairs"].([]any)) != 2 {
		t.Fatalf("boundary inclusive failed: %d %v", code, out)
	}
	// window=2 则不可配，4 条脉冲全跳过。
	code, out = postJSON(t, r, `{"upstream":[0,100],"downstream":[13,113],"delay":10,"window":2,"penalty":1}`)
	if code != 200 || out["cost"].(float64) != 4 || len(out["pairs"].([]any)) != 0 {
		t.Fatalf("outside window should skip: %d %v", code, out)
	}
}

func Test422BatchedAndPositionOrdered(t *testing.T) {
	r := newRouter()
	body := `{"downstream":[1,5,5,9,"x"],"upstream":[2,2],"delay":-5,"window":1000001,"penalty":"abc"}`
	code, out := postJSON(t, r, body)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", code)
	}
	errs := out["errors"].([]any)
	if len(errs) != 6 {
		t.Fatalf("expected 6 errors, got %d: %v", len(errs), errs)
	}
	// position 必须单调不减（按输入位置排序）。
	var prev float64 = -1
	for _, e := range errs {
		em := e.(map[string]any)
		pos := em["position"].(float64)
		if pos < prev {
			t.Fatalf("errors not position-sorted: %v", errs)
		}
		prev = pos
		if em["field"] == "" || em["code"] == "" || em["message"] == "" {
			t.Fatalf("error missing fields: %v", em)
		}
	}
	// 出现 422 时不得携带任何局部结果字段。
	if _, ok := out["cost"]; ok {
		t.Fatalf("422 must not contain cost: %v", out)
	}
	if _, ok := out["pairs"]; ok {
		t.Fatalf("422 must not contain pairs: %v", out)
	}
}

func TestNonIntegerElements(t *testing.T) {
	r := newRouter()
	body := `{"upstream":[1.5,2,"3",true,null,1e2,{}],"downstream":[],"delay":0,"window":0,"penalty":0}`
	code, out := postJSON(t, r, body)
	if code != 422 {
		t.Fatalf("status=%d", code)
	}
	if got := len(out["errors"].([]any)); got != 6 {
		t.Fatalf("expected 6 non-integer errors, got %d: %v", got, out["errors"])
	}
}

func TestMissingAndEmptyBody(t *testing.T) {
	r := newRouter()
	code, out := postJSON(t, r, `{"upstream":[]}`)
	if code != 422 || len(out["errors"].([]any)) != 4 {
		t.Fatalf("missing fields: %d %v", code, out)
	}
	code, out = postJSON(t, r, ``)
	if code != 422 {
		t.Fatalf("empty body status=%d", code)
	}
}

func TestTypeMismatchBatched(t *testing.T) {
	r := newRouter()
	// 同一请求里数组位标量、标量位数组、越界、未知字段都应报出。
	body := `{"upstream":5,"downstream":[1,2],"delay":[1],"window":1000001,"penalty":0,"extra":1}`
	code, out := postJSON(t, r, body)
	if code != 422 {
		t.Fatalf("status=%d", code)
	}
	codes := map[string]string{}
	for _, e := range out["errors"].([]any) {
		em := e.(map[string]any)
		codes[em["field"].(string)] = em["code"].(string)
	}
	want := map[string]string{
		"upstream": "not_an_array",
		"delay":    "not_integer",
		"window":   "out_of_range",
		"extra":    "unknown_field",
	}
	for f, c := range want {
		if codes[f] != c {
			t.Fatalf("field %s: got %q want %q; all=%v", f, codes[f], c, codes)
		}
	}
}

func TestMalformedJSON(t *testing.T) {
	r := newRouter()
	for _, body := range []string{`[1,2]`, `{"upstream":[1,2`, `{"upstream":}`, `garbage`, `{"a":1} x`} {
		code, out := postJSON(t, r, body)
		if code != 422 {
			t.Fatalf("body=%q status=%d want 422", body, code)
		}
		errs := out["errors"].([]any)
		hasInvalid := false
		for _, e := range errs {
			if e.(map[string]any)["code"] == "invalid_json" {
				hasInvalid = true
			}
		}
		if !hasInvalid {
			t.Fatalf("body=%q expected invalid_json among errors, got %v", body, errs)
		}
	}
}

func TestParamBounds(t *testing.T) {
	r := newRouter()
	// 恰好上界合法。
	code, _ := postJSON(t, r, `{"upstream":[],"downstream":[],"delay":1000000,"window":1000000,"penalty":1000000}`)
	if code != 200 {
		t.Fatalf("upper bound should be valid, got %d", code)
	}
	// 超界逐一报错。
	for _, f := range []string{"delay", "window", "penalty"} {
		body := `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":0,"` + f + `":1000001}`
		// 上面的重复键 JSON 不规范，改为构造单字段越界：
		switch f {
		case "delay":
			body = `{"upstream":[],"downstream":[],"delay":1000001,"window":0,"penalty":0}`
		case "window":
			body = `{"upstream":[],"downstream":[],"delay":0,"window":1000001,"penalty":0}`
		case "penalty":
			body = `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":1000001}`
		}
		code, out := postJSON(t, r, body)
		if code != 422 {
			t.Fatalf("%s over bound: status=%d", f, code)
		}
		em := out["errors"].([]any)[0].(map[string]any)
		if em["field"] != f || em["code"] != "out_of_range" {
			t.Fatalf("%s over bound: %v", f, em)
		}
	}
}

func TestIntegerOverflowRejected(t *testing.T) {
	r := newRouter()
	code, out := postJSON(t, r, `{"upstream":[99999999999999999999999],"downstream":[],"delay":0,"window":0,"penalty":0}`)
	if code != 422 || out["errors"].([]any)[0].(map[string]any)["code"] != "not_integer" {
		t.Fatalf("overflow: %d %v", code, out)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	r := newRouter()
	req := httptest.NewRequest(http.MethodGet, "/match", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("GET /match should not be handled as success")
	}
}

func TestDuplicateField(t *testing.T) {
	r := newRouter()
	code, out := postJSON(t, r,
		`{"upstream":[1],"upstream":[2],"downstream":[],"delay":0,"window":0,"penalty":0}`)
	if code != 422 {
		t.Fatalf("status=%d", code)
	}
	found := false
	for _, e := range out["errors"].([]any) {
		if e.(map[string]any)["code"] == "duplicate_field" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected duplicate_field, got %v", out["errors"])
	}
}

func TestBodyTooLarge(t *testing.T) {
	r := newRouter()
	big := bytes.Repeat([]byte(" "), int(maxBodyBytes)+10)
	req := httptest.NewRequest(http.MethodPost, "/match", bytes.NewReader(big))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413", w.Code)
	}
}
