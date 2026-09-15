package match

import (
	"math/rand"
	"reflect"
	"testing"
)

// bruteForce 穷举全部合法的非交叉配对方案，按同样的三级判优选全局最优，
// 仅用于小尺度下交叉验证 Solve。
func bruteForce(up, down []int64, delay, window, penalty int64) (int64, [][2]int) {
	n, m := len(up), len(down)

	var bestCost int64 = 1 << 62
	var bestPairs [][2]int
	matched := 0
	var cur [][2]int

	var rec func(i, j int, cost int64)
	rec = func(i, j int, cost int64) {
		if i == n {
			c := cost + int64(m-j)*penalty
			if accept(c, cur, bestCost, bestPairs) {
				bestCost = c
				bestPairs = append([][2]int(nil), cur...)
			}
			return
		}
		if j == m {
			c := cost + int64(n-i)*penalty
			if accept(c, cur, bestCost, bestPairs) {
				bestCost = c
				bestPairs = append([][2]int(nil), cur...)
			}
			return
		}

		// 跳过 upstream[i]
		rec(i+1, j, cost+penalty)
		// 跳过 downstream[j]
		rec(i, j+1, cost+penalty)
		// 配 (i, j)，需在窗内
		if mc, _, ok := matchCost(up[i], down[j], delay, window); ok {
			cur = append(cur, [2]int{i, j})
			matched++
			rec(i+1, j+1, cost+mc)
			cur = cur[:len(cur)-1]
		}
	}
	rec(0, 0, 0)
	_ = matched
	if bestPairs == nil {
		bestPairs = [][2]int{}
	}
	return bestCost, bestPairs
}

func accept(cost int64, cur [][2]int, bestCost int64, bestPairs [][2]int) bool {
	if cost != bestCost {
		return cost < bestCost
	}
	if len(cur) != len(bestPairs) {
		return len(cur) > len(bestPairs)
	}
	for k := range cur {
		if cur[k] != bestPairs[k] {
			return cur[k][0] < bestPairs[k][0] ||
				(cur[k][0] == bestPairs[k][0] && cur[k][1] < bestPairs[k][1])
		}
	}
	return false
}

func toPairs(ps [][2]int) []Pair {
	out := make([]Pair, 0, len(ps))
	for _, p := range ps {
		out = append(out, Pair{I: p[0], J: p[1]})
	}
	return out
}

func TestAgainstBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20260915))
	for iter := 0; iter < 4000; iter++ {
		n := rng.Intn(6)
		m := rng.Intn(6)
		up := genStrict(rng, n)
		down := genStrict(rng, m)
		delay := int64(rng.Intn(11))
		window := int64(rng.Intn(6))
		penalty := int64(rng.Intn(4)) // 偏小，制造大量并列

		res := Solve(up, down, delay, window, penalty)
		wantCost, wantPairs := bruteForce(up, down, delay, window, penalty)

		if res.Cost != wantCost {
			t.Fatalf("iter=%d cost mismatch: got %d want %d\nup=%v down=%v d=%d w=%d p=%d",
				iter, res.Cost, wantCost, up, down, delay, window, penalty)
		}
		gotIdx := make([][2]int, len(res.Pairs))
		for k, p := range res.Pairs {
			gotIdx[k] = [2]int{p.I, p.J}
		}
		if !reflect.DeepEqual(gotIdx, wantPairs) {
			t.Fatalf("iter=%d pairs mismatch:\n got %v\nwant %v\nup=%v down=%v d=%d w=%d p=%d",
				iter, gotIdx, wantPairs, up, down, delay, window, penalty)
		}
		assertConsistency(t, up, down, delay, window, penalty, res)
	}
}

// TestDenseTieBreaks 针对密集包材（等间隔、强并列）固定几组验证唯一性。
func TestDenseTieBreaks(t *testing.T) {
	cases := []struct {
		up, down               []int64
		delay, window, penalty int64
		wantPairs              [][2]int
	}{
		{
			// 全部残差为 0，penalty=0：代价全 0，取配对数最多（全配）。
			up:    []int64{0, 10, 20, 30},
			down:  []int64{5, 15, 25, 35},
			delay: 5, window: 0, penalty: 0,
			wantPairs: [][2]int{{0, 0}, {1, 1}, {2, 2}, {3, 3}},
		},
		{
			// penalty=0 且窗口宽大：代价恒 0、可全配；等间隔下仍应字典序最小全配。
			up:    []int64{0, 1, 2},
			down:  []int64{0, 1, 2},
			delay: 0, window: 100, penalty: 0,
			wantPairs: [][2]int{{0, 0}, {1, 1}, {2, 2}},
		},
		{
			// 两侧长度不等，penalty=0：配对数取 min(n,m) 且字典序最小。
			up:    []int64{0, 1, 2, 3},
			down:  []int64{0, 1},
			delay: 0, window: 100, penalty: 0,
			wantPairs: [][2]int{{0, 0}, {1, 1}},
		},
		{
			// 空数组。
			up: nil, down: nil, delay: 0, window: 0, penalty: 1,
			wantPairs: [][2]int{},
		},
		{
			// window 很大只代表“允许配对”，配对代价仍为 |残差|。
			// penalty=0 时跳过免费，而此处不存在残差为 0 的配对，
			// 故最小代价 0 由“全跳过”取得（配对数判优只在同代价时生效）。
			up:    []int64{100, 200},
			down:  []int64{300, 400, 500},
			delay: 0, window: 1_000_000, penalty: 0,
			wantPairs: [][2]int{},
		},
	}
	for _, c := range cases {
		res := Solve(c.up, c.down, c.delay, c.window, c.penalty)
		got := make([][2]int, len(res.Pairs))
		for k, p := range res.Pairs {
			got[k] = [2]int{p.I, p.J}
		}
		if len(got) == 0 {
			got = [][2]int{}
		}
		if !reflect.DeepEqual(got, c.wantPairs) {
			t.Fatalf("pairs got %v want %v (up=%v down=%v d=%d w=%d p=%d)",
				got, c.wantPairs, c.up, c.down, c.delay, c.window, c.penalty)
		}
		// 与穷举一致（规模允许时）。
		wc, wp := bruteForce(c.up, c.down, c.delay, c.window, c.penalty)
		if res.Cost != wc || !reflect.DeepEqual(got, wp) {
			t.Fatalf("brute mismatch: got (%d,%v) want (%d,%v)", res.Cost, got, wc, wp)
		}
		assertConsistency(t, c.up, c.down, c.delay, c.window, c.penalty, res)
	}
}

// TestResidualAndCost 校验有符号残差、总代价与未匹配索引的可复算性。
func TestResidualAndCost(t *testing.T) {
	up := []int64{0, 100, 200}
	down := []int64{103, 305}
	res := Solve(up, down, 10, 5, 4)

	// 手算合法配对：
	// (0,0): down103-up0-delay10 = 93 超窗
	// (1,0): 103-100-10 = -7 超窗
	// (2,0): 103-200-10 = -107 超窗 -> downstream0 无法配
	// (0,1): 305-0-10=295 超窗
	// (1,1): 305-100-10=195 超窗
	// (2,1): 305-200-10=95 超窗
	// 全部无法配对 => 5 个脉冲全跳过。
	if len(res.Pairs) != 0 {
		t.Fatalf("expected no pairs, got %v", res.Pairs)
	}
	if res.Cost != int64(5)*4 {
		t.Fatalf("cost = %d, want %d", res.Cost, 20)
	}
	assertConsistency(t, up, down, 10, 5, 4, res)

	// 另一组可配：残差符号两侧都覆盖。
	up2 := []int64{0, 50}
	down2 := []int64{8, 63} // 期望 delay=10：残差 -2 与 +3
	r2 := Solve(up2, down2, 10, 5, 100)
	if r2.Cost != 5 || len(r2.Pairs) != 2 {
		t.Fatalf("unexpected: cost=%d pairs=%v", r2.Cost, r2.Pairs)
	}
	if r2.Pairs[0].Residual != -2 || r2.Pairs[1].Residual != 3 {
		t.Fatalf("residuals = %d,%d want -2,3", r2.Pairs[0].Residual, r2.Pairs[1].Residual)
	}
}

func TestWindowBoundaryInclusive(t *testing.T) {
	// |残差| == window 必须允许（边界可配）。
	up := []int64{0, 100}
	down := []int64{13, 113} // delay=10 => 残差 +3,+3
	res := Solve(up, down, 10, 3, 100)
	if len(res.Pairs) != 2 || res.Cost != 6 {
		t.Fatalf("boundary should match: %+v", res)
	}
	// window 缩小一格则全部不可配，只能跳过。
	res2 := Solve(up, down, 10, 2, 1)
	if len(res2.Pairs) != 0 || res2.Cost != 4 {
		t.Fatalf("outside window must skip: %+v", res2)
	}
}

func TestLargeTimestamps(t *testing.T) {
	up := []int64{1_000_000_000_000, 1_000_000_000_100}
	down := []int64{1_000_000_000_050, 1_000_000_000_150}
	res := Solve(up, down, 50, 0, 10)
	if len(res.Pairs) != 2 || res.Cost != 0 {
		t.Fatalf("large ts mismatch: %+v", res)
	}
}

func genStrict(rng *rand.Rand, n int) []int64 {
	if n == 0 {
		return nil
	}
	a := make([]int64, n)
	v := int64(0)
	for i := range a {
		v += int64(rng.Intn(4)) // 允许相等间隔（密集）
		a[i] = v
	}
	return a
}

func assertConsistency(t *testing.T, up, down []int64, delay, window, penalty int64, res Result) {
	t.Helper()
	// 复算总代价：配对绝对残差 + 两侧跳过脉冲数 * penalty。
	seenUp := map[int]bool{}
	seenDown := map[int]bool{}
	var recompute int64
	for k, p := range res.Pairs {
		if p.I < 0 || p.I >= len(up) || p.J < 0 || p.J >= len(down) {
			t.Fatalf("pair index out of range: %v", p)
		}
		if seenUp[p.I] || seenDown[p.J] {
			t.Fatalf("index reused: %v", res.Pairs)
		}
		seenUp[p.I] = true
		seenDown[p.J] = true
		if k > 0 && (p.I <= res.Pairs[k-1].I || p.J <= res.Pairs[k-1].J) {
			t.Fatalf("pairs not strictly increasing: %v", res.Pairs)
		}
		r := down[p.J] - up[p.I] - delay
		if r != p.Residual {
			t.Fatalf("residual mismatch: got %d want %d", p.Residual, r)
		}
		ar := r
		if ar < 0 {
			ar = -ar
		}
		if ar > window {
			t.Fatalf("pair outside window: %v ar=%d", p, ar)
		}
		recompute += ar
	}
	recompute += int64((len(up)-len(res.Pairs))+(len(down)-len(res.Pairs))) * penalty
	if recompute != res.Cost {
		t.Fatalf("cost not reproducible: got %d recomputed %d", res.Cost, recompute)
	}
	// 未匹配索引与配对互补、升序。
	wantUnUp := []int{}
	for i := range up {
		if !seenUp[i] {
			wantUnUp = append(wantUnUp, i)
		}
	}
	wantUnDown := []int{}
	for j := range down {
		if !seenDown[j] {
			wantUnDown = append(wantUnDown, j)
		}
	}
	if !reflect.DeepEqual(res.UnmatchedUpstream, wantUnUp) ||
		!reflect.DeepEqual(res.UnmatchedDownstream, wantUnDown) {
		t.Fatalf("unmatched mismatch: got (%v,%v) want (%v,%v)",
			res.UnmatchedUpstream, res.UnmatchedDownstream, wantUnUp, wantUnDown)
	}
	if len(res.Pairs) == 0 && res.Pairs == nil {
		t.Fatalf("pairs should be non-nil empty slice")
	}
}
