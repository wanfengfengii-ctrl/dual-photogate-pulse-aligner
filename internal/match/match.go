// Package match 实现两道光电门脉冲的全局顺序配对。
//
// 设上游脉冲 upstream[i]、下游脉冲 downstream[j]（单位均为微秒，非负且
// 严格递增）。一条配对 (i, j) 仅在
//
//	|(downstream[j]-upstream[i])-delay| <= window
//
// 时成立，其代价为该绝对值；跳过任意一条脉冲的代价为 penalty。
//
// 在“顺序不交叉”（被配中的下标沿两侧都严格递增）的全部方案中，依次按
//
//  1. 总代价最小
//  2. 配对数最多
//  3. 按配对顺序组成的 (i, j) 序列字典序最小（索引从零开始）
//
// 三级比较取唯一最优方案。
package match

// Pair 是一条配对结果。Residual 为有符号残差：
// residual = (downstream[J]-upstream[I]) - delay。
type Pair struct {
	I        int   `json:"i"`
	J        int   `json:"j"`
	Residual int64 `json:"residual"`
}

// Result 是一次全局求解的完整结果。
type Result struct {
	Cost                int64  `json:"cost"`
	Pairs               []Pair `json:"pairs"`
	UnmatchedUpstream   []int  `json:"unmatched_upstream"`
	UnmatchedDownstream []int  `json:"unmatched_downstream"`
}

// seq 是一条不可变的配对下标序列（按配对顺序，(i,j) 交错存储）。
//
// 序列通过“父序列 + 末尾一对”的方式增量构造：跳过候选直接复用父格子的
// seq（同一 id），只有配对候选胜出时才生成新节点。id 由构造过程保证
// “同 id 即同内容”，因此等长序列若 id 相同可 O(1) 判等；配对扩展与
// 另一序列比较时，若对方的 parent 就是该父序列 id，则公共前缀必然相同，
// 只需比较末尾一对。密集包材下大量路径对应同一配对序列，借此快速路径
// 避免逐下标扫描；其余情况回退到逐对精确比较。
type seq struct {
	id     uint64  // 内容节点 id（0 为空序列）
	parent uint64  // 父节点 id（空序列为 0）
	x, y   int32   // 本节点追加的一对 (i, j)
	v      []int32 // 完整下标序列（共享只读；配对胜出时复制并追加）
}

// state 是动态规划中一个格子的最优方案。
type state struct {
	cost int64
	s    seq
}

// Solve 执行全局动态规划。入组为已通过校验的非负严格递增时间戳与
// [0,1000000] 范围内的 delay/window/penalty。
//
// 时间复杂度 O(nm) 次状态转移（字典序回退比较最坏再乘以配对数，密集
// 同构场景由 id 快速路径降为 O(1)）；空间复杂度为两行状态。
func Solve(upstream, downstream []int64, delay, window, penalty int64) Result {
	n, m := len(upstream), len(downstream)

	var nextID uint64
	empty := seq{id: 0, v: []int32{}}

	prev := make([]state, m+1)
	// 第 0 行：上游脉冲尚不可用，只能逐条跳过下游脉冲（序列始终为空）。
	for j := 1; j <= m; j++ {
		prev[j] = state{cost: int64(j) * penalty, s: empty}
	}

	for i := 1; i <= n; i++ {
		cur := make([]state, m+1)

		// 第 0 列：只能逐条跳过上游脉冲。
		cur[0] = state{cost: int64(i) * penalty, s: empty}

		for j := 1; j <= m; j++ {
			// 候选 1：跳过 upstream[i-1]（从上方来）。
			bestCost := prev[j].cost + penalty
			bestSeq := prev[j].s

			// 候选 2：跳过 downstream[j-1]（从左方来）。
			leftCost := cur[j-1].cost + penalty
			if better(leftCost, cur[j-1].s, bestCost, bestSeq) {
				bestCost = leftCost
				bestSeq = cur[j-1].s
			}

			// 候选 3：配 (i-1, j-1)（从对角来），需落在时间窗内。
			if mc, _, ok := matchCost(upstream[i-1], downstream[j-1], delay, window); ok {
				diagCost := prev[j-1].cost + mc
				if betterExt(prev[j-1].s, int32(i-1), int32(j-1), diagCost, bestCost, bestSeq) {
					nextID++
					v := make([]int32, len(prev[j-1].s.v)+2)
					copy(v, prev[j-1].s.v)
					v[len(v)-2] = int32(i - 1)
					v[len(v)-1] = int32(j - 1)
					bestCost = diagCost
					bestSeq = seq{
						id:     nextID,
						parent: prev[j-1].s.id,
						x:      int32(i - 1),
						y:      int32(j - 1),
						v:      v,
					}
				}
			}

			cur[j] = state{cost: bestCost, s: bestSeq}
		}
		prev = cur
	}

	final := prev[m].s.v

	pairs := make([]Pair, 0, len(final)/2)
	usedUp := make([]bool, n)
	usedDown := make([]bool, m)
	for k := 0; k+1 < len(final); k += 2 {
		ii, jj := int(final[k]), int(final[k+1])
		usedUp[ii] = true
		usedDown[jj] = true
		pairs = append(pairs, Pair{
			I:        ii,
			J:        jj,
			Residual: downstream[jj] - upstream[ii] - delay,
		})
	}

	unUp := make([]int, 0, n)
	unDown := make([]int, 0, m)
	for i, used := range usedUp {
		if !used {
			unUp = append(unUp, i)
		}
	}
	for j, used := range usedDown {
		if !used {
			unDown = append(unDown, j)
		}
	}

	return Result{
		Cost:                prev[m].cost,
		Pairs:               pairs,
		UnmatchedUpstream:   unUp,
		UnmatchedDownstream: unDown,
	}
}

// better 判断方案 a 是否严格优于方案 b。
// 判优顺序：总代价 -> 配对数 -> (i,j) 序列字典序。
// 三级全部相同时视为等价（返回 false，保持先入候选，二者配对结果一致）。
func better(ac int64, a seq, bc int64, b seq) bool {
	if ac != bc {
		return ac < bc
	}
	if len(a.v) != len(b.v) {
		return len(a.v) > len(b.v) // 序列长度 = 2*配对数
	}
	if a.id == b.id {
		return false // 同 id 必为同一配对序列
	}
	return compareSeq(a.v, b.v) < 0
}

// betterExt 判断“父序列 p 再追加一对 (x,y)，代价 ac”是否严格优于
// 方案 b（代价 bc、序列 b）。按需逐对比较，不物化新序列。
func betterExt(p seq, x, y int32, ac int64, bc int64, b seq) bool {
	if ac != bc {
		return ac < bc
	}
	la, lb := len(p.v)+2, len(b.v)
	if la != lb {
		return la > lb
	}
	// 对方正是在同一父序列上追加了一对：公共前缀构造上相同，
	// 只需字典序比较末尾一对。
	if b.parent == p.id && len(b.v) == la {
		if x != b.x {
			return x < b.x
		}
		return y < b.y
	}
	// 一般情况：精确比较公共前缀，再比较末尾一对。
	if c := compareSeq(p.v, b.v[:len(p.v)]); c != 0 {
		return c < 0
	}
	if x != b.v[lb-2] {
		return x < b.v[lb-2]
	}
	return y < b.v[lb-1]
}

// compareSeq 按配对顺序字典序比较两条下标序列（元素成对比较）。
func compareSeq(a, b []int32) int {
	for k := 0; k+1 < len(a); k += 2 {
		if a[k] != b[k] {
			if a[k] < b[k] {
				return -1
			}
			return 1
		}
		if a[k+1] != b[k+1] {
			if a[k+1] < b[k+1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// matchCost 返回配对的绝对残差代价与有符号残差；窗口外返回 ok=false。
// delay、window 均不超过 1e6，先用窄区间判定，避免大时间戳差值的边界问题。
func matchCost(up, down, delay, window int64) (cost int64, residual int64, ok bool) {
	d := down - up // 两侧均非负，差在 int64 范围内
	// delay-window >= -1e6，delay+window <= 2e6，区间之外绝无可能命中。
	if d < -1_000_000 || d > 2_000_000 {
		return 0, 0, false
	}
	r := d - delay
	if r < 0 {
		cost = -r
	} else {
		cost = r
	}
	if cost > window {
		return 0, r, false
	}
	return cost, r, true
}
