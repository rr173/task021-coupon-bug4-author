// Package coupon 实现优惠券规则引擎：按固定叠加顺序结算订单与一批优惠券，
// 计算应付金额、命中与跳过明细，并支持在满足互斥约束的组合中推荐最优方案。
// 所有判定只依据入参本身，不持久化、不记录历史。
package coupon

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CouponType 标识券的类型。
type CouponType string

const (
	// CouponItemBundle 满件折：指定 SKU 达到门槛件数后按折扣率重新计价。
	CouponItemBundle CouponType = "ITEM_BUNDLE"
	// CouponDiscount 订单折扣：对整单或指定品类小计按折扣率计价。
	CouponDiscount CouponType = "DISCOUNT"
	// CouponFullReduction 满减：当前总额达门槛则减固定金额。
	CouponFullReduction CouponType = "FULL_REDUCTION"
	// CouponFlat 无门槛直减：直接减固定金额。
	CouponFlat CouponType = "FLAT"
)

// Item 是订单的一个商品行。UnitPrice 与小计均以"分"为单位。
type Item struct {
	SKU       string `json:"sku"`
	Category  string `json:"category"`
	UnitPrice int64  `json:"unit_price"`
	Qty       int    `json:"qty"`
}

// Coupon 是一张优惠券。各类型只使用与自身相关的字段，其余字段为零值。
type Coupon struct {
	ID             string     `json:"id"`
	Type           CouponType `json:"type"`
	ExclusiveGroup string     `json:"exclusive_group"`

	// FULL_REDUCTION / FLAT
	Threshold int64 `json:"threshold"` // 满减门槛（分）
	Amount    int64 `json:"amount"`    // 减额（分）

	// DISCOUNT
	RateBps  int    `json:"rate_bps"` // 折扣率（基点，10000=不打折）
	Category string `json:"category"` // 空表示整单

	// ITEM_BUNDLE
	SKU           string `json:"sku"`
	BundleQty     int    `json:"bundle_qty"`      // 满件门槛
	BundleRateBps int    `json:"bundle_rate_bps"` // 命中后的折扣率（基点）
}

// Order 是规则引擎的输入：商品行、可用券与底价保护率。
type Order struct {
	Items        []Item   `json:"items"`
	Coupons      []Coupon `json:"coupons"`
	FloorRateBps int      `json:"floor_rate_bps"`
}

// AppliedCoupon 记录一张生效的券及其造成的实际减免。
type AppliedCoupon struct {
	ID        string     `json:"id"`
	Type      CouponType `json:"type"`
	Reduction int64      `json:"reduction"`
}

// SkippedCoupon 记录一张被跳过的券及其原因。
type SkippedCoupon struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Result 是结算结果。Chosen 仅在推荐端点填充。
type Result struct {
	OriginalTotal int64           `json:"original_total"`
	Floor         int64           `json:"floor"`
	Payable       int64           `json:"payable"`
	Applied       []AppliedCoupon `json:"applied"`
	Skipped       []SkippedCoupon `json:"skipped"`
	Chosen        []string        `json:"chosen,omitempty"`
}

// MaxCouponsForRecommend 是推荐端点允许的可用券数量上限。
const MaxCouponsForRecommend = 16

// ErrTooManyCoupons 表示推荐端点收到的可用券超过上限。
var ErrTooManyCoupons = errors.New("可用券数量超过上限 16")

// mulBps 按 bps 基点对 amount（分）计价，四舍五入到分（half-up）。
// bps=10000 时返回 amount（不打折），bps<=0 或 amount<=0 时返回 0。
func mulBps(amount int64, bps int) int64 {
	if amount <= 0 || bps <= 0 {
		return 0
	}
	return (amount * int64(bps)) / 10000
}

// scopeLabel 把作用域标签转成可读文字用于错误信息。
func scopeLabel(scope string) string {
	if scope == "" {
		return "整单"
	}
	return scope
}

// apply 向结果追加一张生效券。
func (r *Result) apply(id string, t CouponType, reduction int64) {
	r.Applied = append(r.Applied, AppliedCoupon{ID: id, Type: t, Reduction: reduction})
}

// skip 向结果追加一张跳过券。
func (r *Result) skip(id, reason string) {
	r.Skipped = append(r.Skipped, SkippedCoupon{ID: id, Reason: reason})
}

// Validate 校验订单的结构合法性。返回的错误对应 HTTP 400。
func Validate(o Order) error {
	if len(o.Items) == 0 {
		return errors.New("订单商品行不能为空")
	}
	seenSKU := make(map[string]bool, len(o.Items))
	for i, it := range o.Items {
		if strings.TrimSpace(it.SKU) == "" {
			return fmt.Errorf("第 %d 个商品行 SKU 为空", i)
		}
		if seenSKU[it.SKU] {
			return fmt.Errorf("商品行 SKU 重复：%q", it.SKU)
		}
		seenSKU[it.SKU] = true
		if it.UnitPrice < 0 {
			return fmt.Errorf("商品 %q 单价为负", it.SKU)
		}
		if it.Qty < 1 {
			return fmt.Errorf("商品 %q 数量须 >= 1", it.SKU)
		}
	}
	if o.FloorRateBps < 0 || o.FloorRateBps > 10000 {
		return fmt.Errorf("底价保护率 %d 须在 [0,10000]", o.FloorRateBps)
	}
	seenID := make(map[string]bool, len(o.Coupons))
	for i, c := range o.Coupons {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("第 %d 张券 ID 为空", i)
		}
		if seenID[c.ID] {
			return fmt.Errorf("券 ID 重复：%q", c.ID)
		}
		seenID[c.ID] = true
		switch c.Type {
		case CouponItemBundle, CouponDiscount, CouponFullReduction, CouponFlat:
		default:
			return fmt.Errorf("券 %q 类型未知：%q", c.ID, c.Type)
		}
	}
	return nil
}

// CheckExclusive 检查券集合是否违反互斥组约束：同一非空互斥组内不得多于一张。
// 返回的错误对应 HTTP 400（仅对显式结算端点有意义；推荐端点自行枚举合法子集）。
func CheckExclusive(coupons []Coupon) error {
	return nil
}

// runPipeline 在给定商品行与券集合上执行四阶段结算，返回结果（不报错；
// 单券的可用性问题进入 skipped）。四阶段顺序固定：
//  1. ITEM_BUNDLE：改写命中 SKU 的有效小计（同 SKU 首张命中，余者跳过）。
//  2. DISCOUNT：基数为满件折后的总额/品类小计快照；同作用域首张生效，余者跳过。
//  3. FULL_REDUCTION：门槛以当前累计应付判定（动态门槛）。
//  4. FLAT：直减。
//
// 每张券的减免不超过当前剩余应付；最终以底价保护抬升（仅抬升不下调）。
func runPipeline(items []Item, coupons []Coupon, floorRateBps int) *Result {
	type line struct {
		item Item
		orig int64
		eff  int64
	}
	lines := make([]line, len(items))
	idx := make(map[string]int, len(items))
	var originalTotal int64
	for i, it := range items {
		orig := it.UnitPrice * int64(it.Qty)
		lines[i] = line{item: it, orig: orig, eff: orig}
		idx[it.SKU] = i
		originalTotal += orig
	}
	floor := mulBps(originalTotal, floorRateBps)

	res := &Result{
		OriginalTotal: originalTotal,
		Floor:         floor,
		Applied:       []AppliedCoupon{},
		Skipped:       []SkippedCoupon{},
	}
	currentTotal := originalTotal

	// 按阶段分组，组内保持传入顺序。
	var bundles, discounts, fulls, flats []Coupon
	for _, c := range coupons {
		switch c.Type {
		case CouponItemBundle:
			bundles = append(bundles, c)
		case CouponDiscount:
			discounts = append(discounts, c)
		case CouponFullReduction:
			fulls = append(fulls, c)
		case CouponFlat:
			flats = append(flats, c)
		}
	}

	// 阶段 1：满件折。
	bundled := make(map[string]bool)
	for _, c := range bundles {
		if c.BundleRateBps <= 0 || c.BundleRateBps >= 10000 {
			res.skip(c.ID, "满件折折扣率须在 (0,10000)")
			continue
		}
		li, ok := idx[c.SKU]
		if !ok {
			res.skip(c.ID, fmt.Sprintf("SKU %q 不在订单中", c.SKU))
			continue
		}
		if lines[li].item.Qty < c.BundleQty {
			res.skip(c.ID, fmt.Sprintf("未达满件门槛 %d（实际 %d）", c.BundleQty, lines[li].item.Qty))
			continue
		}
		if bundled[c.SKU] {
			res.skip(c.ID, fmt.Sprintf("SKU %q 已被满件折命中", c.SKU))
			continue
		}
		old := lines[li].eff
		nw := mulBps(lines[li].orig, c.BundleRateBps)
		lines[li].eff = nw
		currentTotal += nw - old
		bundled[c.SKU] = true
		res.apply(c.ID, c.Type, old-nw)
	}

	// 阶段 2 基数快照：满件折后的有效小计与总额。
	effSnap := make([]int64, len(lines))
	for i := range lines {
		effSnap[i] = lines[i].eff
	}
	discountBaseTotal := currentTotal

	// 阶段 2：订单折扣。
	seenScope := make(map[string]bool)
	for _, c := range discounts {
		if c.RateBps <= 0 || c.RateBps >= 10000 {
			res.skip(c.ID, "折扣率须在 (0,10000)")
			continue
		}
		scope := c.Category
		if seenScope[scope] {
			res.skip(c.ID, fmt.Sprintf("作用域 %q 的折扣券重复", scopeLabel(scope)))
			continue
		}
		seenScope[scope] = true
		var base int64
		if scope == "" {
			base = discountBaseTotal
		} else {
			for i := range lines {
				if lines[i].item.Category == scope {
					base += lines[i].orig
				}
			}
		}
		if base <= 0 {
			res.skip(c.ID, "无可折扣金额")
			continue
		}
		reduction := base - mulBps(base, c.RateBps)
		if reduction > currentTotal {
			reduction = currentTotal
		}
		currentTotal -= reduction
		res.apply(c.ID, c.Type, reduction)
	}

	// 阶段 3：满减（动态门槛）。
	for _, c := range fulls {
		if c.Amount <= 0 {
			res.skip(c.ID, "满减金额须为正")
			continue
		}
		if c.Threshold < 0 {
			res.skip(c.ID, "满减门槛为负")
			continue
		}
		if currentTotal < c.Threshold {
			res.skip(c.ID, fmt.Sprintf("未达满减门槛 %d（当前 %d）", c.Threshold, currentTotal))
			continue
		}
		reduction := c.Amount
		if reduction > currentTotal {
			reduction = currentTotal
		}
		currentTotal -= reduction
		res.apply(c.ID, c.Type, reduction)
	}

	// 阶段 4：无门槛直减。
	for _, c := range flats {
		if c.Amount <= 0 {
			res.skip(c.ID, "直减金额须为正")
			continue
		}
		reduction := c.Amount
		if reduction > currentTotal {
			reduction = currentTotal
		}
		currentTotal -= reduction
		res.apply(c.ID, c.Type, reduction)
	}

	// 底价保护：仅最终抬升一次。
	if currentTotal < floor {
		currentTotal = floor
	}
	res.Payable = currentTotal
	return res
}

// Apply 校验并按固定顺序结算全部券。互斥组冲突返回错误（HTTP 400）。
func Apply(o Order) (*Result, error) {
	if err := Validate(o); err != nil {
		return nil, err
	}
	if err := CheckExclusive(o.Coupons); err != nil {
		return nil, err
	}
	return runPipeline(o.Items, o.Coupons, o.FloorRateBps), nil
}

// withCoupon 返回 base 追加 c 后的副本，避免递归共享底层数组。
func withCoupon(base []Coupon, c Coupon) []Coupon {
	out := make([]Coupon, len(base)+1)
	copy(out, base)
	out[len(base)] = c
	return out
}

// lexLess 报告 a 是否字典序小于 b（假设两者已排序）。
func lexLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return len(a) < len(b)
}

// Recommend 在所有满足互斥约束的券子集中，选出使应付金额最低的组合。
// 应付相同时优先选中券数更少者，再按券 ID 字典序取最小。
func Recommend(o Order) (*Result, error) {
	if err := Validate(o); err != nil {
		return nil, err
	}
	if len(o.Coupons) > MaxCouponsForRecommend {
		return nil, ErrTooManyCoupons
	}

	var ungrouped []Coupon
	groups := make(map[string][]Coupon)
	var groupKeys []string
	for _, c := range o.Coupons {
		if c.ExclusiveGroup == "" {
			ungrouped = append(ungrouped, c)
		} else {
			if _, ok := groups[c.ExclusiveGroup]; !ok {
				groupKeys = append(groupKeys, c.ExclusiveGroup)
			}
			groups[c.ExclusiveGroup] = append(groups[c.ExclusiveGroup], c)
		}
	}

	// 组合数上限保护：2^|ungrouped| * ∏(|group|+1)。
	combos := int64(1) << len(ungrouped)
	for _, k := range groupKeys {
		combos *= int64(len(groups[k]) + 1)
		if combos > 65536 {
			return nil, errors.New("组合数过多，超过 65536")
		}
	}

	var best *Result
	var bestChosen []string

	// enumerate 对每个互斥组取"零张或一张"，再对无组券做子集枚举。
	var enumerate func(gi int, chosen []Coupon)
	enumerate = func(gi int, chosen []Coupon) {
		if gi == len(groupKeys) {
			n := len(ungrouped)
			for mask := 0; mask < (1 << n); mask++ {
				subset := make([]Coupon, len(chosen))
				copy(subset, chosen)
				for i := 0; i < n; i++ {
					if mask&(1<<i) != 0 {
						subset = append(subset, ungrouped[i])
					}
				}
				r := runPipeline(o.Items, subset, o.FloorRateBps)
				ids := make([]string, 0, len(subset))
				for _, c := range subset {
					ids = append(ids, c.ID)
				}
				sort.Strings(ids)

				better := false
				switch {
				case best == nil:
					better = true
				case r.Payable < best.Payable:
					better = true
				case r.Payable == best.Payable:
					if len(ids) < len(bestChosen) {
						better = true
					} else if len(ids) == len(bestChosen) && lexLess(ids, bestChosen) {
						better = true
					}
				}
				if better {
					best = r
					bestChosen = ids
				}
			}
			return
		}
		k := groupKeys[gi]
		enumerate(gi+1, chosen) // 该组不选
		for _, c := range groups[k] {
			enumerate(gi+1, withCoupon(chosen, c))
		}
	}
	enumerate(0, nil)

	best.Chosen = bestChosen
	if best.Chosen == nil {
		best.Chosen = []string{}
	}
	return best, nil
}
