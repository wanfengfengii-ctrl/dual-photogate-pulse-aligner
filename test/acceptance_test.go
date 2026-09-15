// Package test 是面向真实运行中 API 的端到端验收测试。
// 它在 verify 一次性容器内运行，通过 MATCH_API_BASE_URL 访问 api 服务；
// 本地也可对已启动的 server 运行：
//
//	MATCH_API_BASE_URL=http://localhost:8080 go test ./test/...
package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"testing"
	"time"
)

var baseURL string

type pair struct {
	I        int   `json:"i"`
	J        int   `json:"j"`
	Residual int64 `json:"residual"`
}

type matchResponse struct {
	Cost                int64  `json:"cost"`
	Pairs               []pair `json:"pairs"`
	UnmatchedUpstream   []int  `json:"unmatched_upstream"`
	UnmatchedDownstream []int  `json:"unmatched_downstream"`
}

type fieldError struct {
	Position int64  `json:"position"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type errorResponse struct {
	Errors []fieldError `json:"errors"`
}

func TestMain(m *testing.M) {
	baseURL = os.Getenv("MATCH_API_BASE_URL")
	if baseURL == "" {
		// 未显式指定待验收 API 时跳过；verify 容器总会注入该变量。
		fmt.Println("SKIP: MATCH_API_BASE_URL not set; point it at a running API to run acceptance tests")
		return
	}
	if err := waitReady(baseURL, 40*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "API not ready at %s: %v\n", baseURL, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// waitReady 通过 TCP 拨号轮询，直到 API 端口可连，不依赖额外的健康检查路由。
func waitReady(base string, timeout time.Duration) error {
	host, err := urlHostPort(base)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", host, time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", host)
}

// urlHostPort 从 http(s)://host:port 形式的 base URL 提取 host:port，
// 缺省端口按协议补 80/443。
func urlHostPort(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid base URL %q", base)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

func postMatch(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/match", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http call: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func mustMatch(t *testing.T, body string) matchResponse {
	t.Helper()
	code, raw := postMatch(t, body)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var r matchResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode: %v body=%s", err, raw)
	}
	return r
}

func mustErrors(t *testing.T, body string) (int, errorResponse) {
	t.Helper()
	code, raw := postMatch(t, body)
	var e errorResponse
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode errors: %v body=%s", err, raw)
	}
	return code, e
}

// assertReproducible 依据返回结果独立复算总代价，并校验配对窗内、
// 严格递增、下标与未匹配集合互补。
func assertReproducible(t *testing.T, up, down []int64, delay, window, penalty int64, r matchResponse) {
	t.Helper()
	usedUp := map[int]bool{}
	usedDown := map[int]bool{}
	var recomputed int64
	for k, p := range r.Pairs {
		if p.I < 0 || p.I >= len(up) || p.J < 0 || p.J >= len(down) {
			t.Fatalf("pair index out of range: %+v", p)
		}
		if usedUp[p.I] || usedDown[p.J] {
			t.Fatalf("index reused: %+v", r.Pairs)
		}
		usedUp[p.I] = true
		usedDown[p.J] = true
		if k > 0 && (p.I <= r.Pairs[k-1].I || p.J <= r.Pairs[k-1].J) {
			t.Fatalf("pairs not strictly increasing: %+v", r.Pairs)
		}
		want := down[p.J] - up[p.I] - delay
		if want != p.Residual {
			t.Fatalf("residual mismatch: got %d want %d", p.Residual, want)
		}
		abs := want
		if abs < 0 {
			abs = -abs
		}
		if abs > window {
			t.Fatalf("pair outside window: %+v abs=%d", p, abs)
		}
		recomputed += abs
	}
	recomputed += int64(len(up)+len(down)-2*len(r.Pairs)) * penalty
	if recomputed != r.Cost {
		t.Fatalf("cost not reproducible: got %d recomputed %d", r.Cost, recomputed)
	}

	wantUnUp := make([]int, 0, len(up))
	for i := range up {
		if !usedUp[i] {
			wantUnUp = append(wantUnUp, i)
		}
	}
	wantUnDown := make([]int, 0, len(down))
	for j := range down {
		if !usedDown[j] {
			wantUnDown = append(wantUnDown, j)
		}
	}
	if !eqInts(r.UnmatchedUpstream, wantUnUp) || !eqInts(r.UnmatchedDownstream, wantUnDown) {
		t.Fatalf("unmatched mismatch: got (%v,%v) want (%v,%v)",
			r.UnmatchedUpstream, r.UnmatchedDownstream, wantUnUp, wantUnDown)
	}
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBasicMatchAndSignedResiduals(t *testing.T) {
	body := `{"upstream":[0,50],"downstream":[8,63],"delay":10,"window":5,"penalty":100}`
	r := mustMatch(t, body)
	if r.Cost != 5 || len(r.Pairs) != 2 {
		t.Fatalf("unexpected: %+v", r)
	}
	if r.Pairs[0].Residual != -2 || r.Pairs[1].Residual != 3 {
		t.Fatalf("signed residuals wrong: %+v", r.Pairs)
	}
	assertReproducible(t, []int64{0, 50}, []int64{8, 63}, 10, 5, 100, r)
}

func TestWindowBoundaryInclusive(t *testing.T) {
	// 残差恰为 +3：window=3 边界内必须可配。
	r := mustMatch(t, `{"upstream":[0,100],"downstream":[13,113],"delay":10,"window":3,"penalty":100}`)
	if r.Cost != 6 || len(r.Pairs) != 2 {
		t.Fatalf("boundary must match: %+v", r)
	}
	// window 缩小为 2 后全部超窗，只能跳过（penalty=1，共 4 条脉冲）。
	r2 := mustMatch(t, `{"upstream":[0,100],"downstream":[13,113],"delay":10,"window":2,"penalty":1}`)
	if r2.Cost != 4 || len(r2.Pairs) != 0 {
		t.Fatalf("outside window must skip: %+v", r2)
	}
}

func TestEmptyArraysAllowed(t *testing.T) {
	r := mustMatch(t, `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":1}`)
	if r.Cost != 0 || len(r.Pairs) != 0 {
		t.Fatalf("empty input: %+v", r)
	}
	// JSON 中必须输出 [] 而不是 null。
	_, raw := postMatch(t, `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":1}`)
	if !bytes.Contains(raw, []byte(`"pairs":[]`)) {
		t.Fatalf("pairs should serialize as []: %s", raw)
	}
	// 单侧为空：所有上游脉冲都计入未匹配。
	r2 := mustMatch(t, `{"upstream":[1,2,3],"downstream":[],"delay":0,"window":0,"penalty":2}`)
	if r2.Cost != 6 || !eqInts(r2.UnmatchedUpstream, []int{0, 1, 2}) {
		t.Fatalf("one-sided empty: %+v", r2)
	}
}

func TestDensePulsesYieldUniqueReproducibleMatch(t *testing.T) {
	// 密集包材：间隔 10、delay=10、window=9（除对角对齐外，多种错位也在窗内），
	// penalty=0 使大量方案代价并列；三级判优后必须得到唯一、可复算的结果。
	up := []int64{0, 10, 20, 30, 40, 50, 60, 70}
	down := []int64{10, 20, 30, 40, 50, 60, 70, 80}
	body := `{"upstream":[0,10,20,30,40,50,60,70],"downstream":[10,20,30,40,50,60,70,80],` +
		`"delay":10,"window":9,"penalty":0}`

	first := mustMatch(t, body)
	// 再请求一次：必须逐字节级稳定（这里比较结构）。
	second := mustMatch(t, body)
	if !samePairs(first.Pairs, second.Pairs) || first.Cost != second.Cost {
		t.Fatalf("non-deterministic result:\n%+v\n%+v", first, second)
	}

	// 残差全 0 的对角对齐同时实现：最小代价 0、配对数最多 8、字典序最小。
	if first.Cost != 0 || len(first.Pairs) != 8 {
		t.Fatalf("dense result: %+v", first)
	}
	for k, p := range first.Pairs {
		if p.I != k || p.J != k || p.Residual != 0 {
			t.Fatalf("expected diagonal pairs (k,k), got %+v", first.Pairs)
		}
	}
	assertReproducible(t, up, down, 10, 9, 0, first)
}

// TestDenseWithNoise 模拟漏检/毛刺/密集并存：两侧长度不同且含可跳过噪声，
// 结果仍须唯一、可复算，并对随机输入满足基本不变量。
func TestDenseWithNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 60; iter++ {
		n := 1 + rng.Intn(12)
		m := 1 + rng.Intn(12)
		up := make([]int64, n)
		down := make([]int64, m)
		var v int64
		for i := range up {
			v += int64(1 + rng.Intn(3))
			up[i] = v
		}
		v = 8 // 近似 delay
		for j := range down {
			v += int64(1 + rng.Intn(3))
			down[j] = v
		}
		delay := int64(6 + rng.Intn(6))
		window := int64(rng.Intn(5))
		penalty := int64(rng.Intn(4))

		payload, _ := json.Marshal(map[string]any{
			"upstream": up, "downstream": down,
			"delay": delay, "window": window, "penalty": penalty,
		})
		r1 := mustMatch(t, string(payload))
		r2 := mustMatch(t, string(payload))
		if !samePairs(r1.Pairs, r2.Pairs) || r1.Cost != r2.Cost {
			t.Fatalf("iter=%d nondeterministic", iter)
		}
		assertReproducible(t, up, down, delay, window, penalty, r1)
	}
}

func Test422BatchedPositionSortedNoPartialResult(t *testing.T) {
	body := `{"downstream":[1,5,5,9,"x"],"upstream":[2,2],"delay":-5,"window":1000001,"penalty":"abc"}`
	code, raw := postMatch(t, body)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	var er errorResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(er.Errors) != 6 {
		t.Fatalf("expected 6 errors, got %d: %s", len(er.Errors), raw)
	}
	if !sort.SliceIsSorted(er.Errors, func(i, j int) bool {
		if er.Errors[i].Position != er.Errors[j].Position {
			return er.Errors[i].Position < er.Errors[j].Position
		}
		return er.Errors[i].Field < er.Errors[j].Field
	}) {
		t.Fatalf("errors not sorted by input position: %+v", er.Errors)
	}
	for _, e := range er.Errors {
		if e.Field == "" || e.Code == "" || e.Message == "" {
			t.Fatalf("error field incomplete: %+v", e)
		}
	}
	// 禁止局部结果：错误响应里不得出现 cost/pairs。
	if bytes.Contains(raw, []byte(`"cost"`)) || bytes.Contains(raw, []byte(`"pairs"`)) {
		t.Fatalf("422 must not contain partial results: %s", raw)
	}
}

func Test422NonIntegerAndNotIncreasing(t *testing.T) {
	code, er := mustErrors(t, `{"upstream":[1,"2",1.5,true,null],"downstream":[],"delay":0,"window":0,"penalty":0}`)
	if code != 422 {
		t.Fatalf("status=%d", code)
	}
	if len(er.Errors) != 4 { // "2"、1.5、true、null 四个非整数
		t.Fatalf("got %d errors: %+v", len(er.Errors), er.Errors)
	}
	code, er = mustErrors(t, `{"upstream":[3,2],"downstream":[],"delay":0,"window":0,"penalty":0}`)
	if code != 422 || er.Errors[0].Code != "not_strictly_increasing" {
		t.Fatalf("strict increase: %d %+v", code, er.Errors)
	}
}

func Test422ParameterRange(t *testing.T) {
	// 上界合法。
	if code, _ := postMatch(t, `{"upstream":[],"downstream":[],"delay":1000000,"window":1000000,"penalty":1000000}`); code != 200 {
		t.Fatalf("upper bound must be valid, got %d", code)
	}
	// 越界整批拒绝。
	code, er := mustErrors(t, `{"upstream":[],"downstream":[],"delay":0,"window":0,"penalty":1000001}`)
	if code != 422 || len(er.Errors) != 1 || er.Errors[0].Field != "penalty" {
		t.Fatalf("range: %d %+v", code, er.Errors)
	}
}

func samePairs(a, b []pair) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
