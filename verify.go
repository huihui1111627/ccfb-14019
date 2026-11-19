package main

import (
	"fmt"
	"sort"
	"strings"
)

// 验收器:按拓扑 + 单播最短路独立算出"应有的树",和每台路由器实际转发表对比。
//  - RPT:每个接收者网段的 DR 沿 RPF(RP) 一路到 RP 的路径并集
//  - SPT:最后一跳沿 RPF(S) 到源第一跳的路径并集(切换后);
//        未切换时 SPT 只到 RP(Register 触发的那一段)
//  - 流量检查:统计窗口内每个网段上该流的实际传输,必须 ⊆ 树上的网段

type Expect struct {
	RPTOifs    map[string]map[string]bool // 路由器 -> (*,G) 结构出接口名
	StreamOifs map[string]map[string]bool // 路由器 -> 该 (S,G) 流的有效出接口名
	TrafficSeg map[string]bool            // 该 (S,G) 应经过的网段
}

func drOf(t *Topo, seg *Seg) *Router {
	var best *Router
	for _, if_ := range seg.Ifs {
		if best == nil || if_.R.ID > best.ID { // 优先级相同,比大 ID
			best = if_.R
		}
	}
	return best
}

func ifName(if_ *Iface) string {
	if if_ == nil {
		return "-"
	}
	return if_.Name()
}

// 沿 RPF 从 from 走到 target(路由器 ID 或地址),沿途记录:
// 上游路由器的出接口、经过的网段。返回是否可达。
func walkRPF(t *Topo, from *Router, target string, oifs map[string]map[string]bool, segs map[string]bool) bool {
	cur := from
	owner := t.owner[target]
	for cur.ID != owner {
		ri := t.RPF(cur, target)
		if ri.If == nil {
			return false
		}
		segs[ri.If.Seg.Name] = true
		nbr := t.Routers[ri.Nbr]
		if nbr == nil {
			break // 目标是直连主机/源
		}
		addOif(oifs, nbr.ID, ifOnSeg(nbr, ri.If.Seg).Name())
		cur = nbr
	}
	return true
}

func addOif(m map[string]map[string]bool, r, ifn string) {
	if m[r] == nil {
		m[r] = map[string]bool{}
	}
	m[r][ifn] = true
}

// ExpectRPT 只算 (*,G) 共享树结构(接收者加入即存在,与源无关)
func ExpectRPT(t *Topo, g string, receivers []*Host) *Expect {
	e := &Expect{
		RPTOifs:    map[string]map[string]bool{},
		StreamOifs: map[string]map[string]bool{},
		TrafficSeg: map[string]bool{},
	}
	for _, h := range receivers {
		if !h.joined[g] {
			continue
		}
		dr := drOf(t, h.Seg)
		addOif(e.RPTOifs, dr.ID, ifOnSeg(dr, h.Seg).Name())
		e.TrafficSeg[h.Seg.Name] = true
		walkRPF(t, dr, t.RP, e.RPTOifs, e.TrafficSeg)
	}
	return e
}

// ExpectStream 算某条 (S,G) 流的应有转发树。switched=最后一跳是否已切 SPT。
func ExpectStream(t *Topo, key SG, receivers []*Host, switched bool) *Expect {
	e := &Expect{
		RPTOifs:    map[string]map[string]bool{},
		StreamOifs: map[string]map[string]bool{},
		TrafficSeg: map[string]bool{},
	}
	src := t.Sources[key.S]
	if src == nil || !src.Active {
		return e
	}
	e.TrafficSeg[src.Seg.Name] = true // 源直联网段必有流量
	rpt := ExpectRPT(t, key.G, receivers)
	e.RPTOifs = rpt.RPTOifs
	if !switched {
		// 共享树 + Register 触发的源到 RP 的 (S,G) 树
		for r, set := range rpt.RPTOifs {
			for ifn := range set {
				addOif(e.StreamOifs, r, ifn)
			}
		}
		for s := range rpt.TrafficSeg {
			e.TrafficSeg[s] = true
		}
		walkRPF(t, t.Routers[t.RP], key.S, e.StreamOifs, e.TrafficSeg)
		return e
	}
	// 切换后:流量只走 SPT(源 -> 各最后一跳 DR)
	for _, h := range receivers {
		if !h.joined[key.G] {
			continue
		}
		dr := drOf(t, h.Seg)
		addOif(e.StreamOifs, dr.ID, ifOnSeg(dr, h.Seg).Name())
		e.TrafficSeg[h.Seg.Name] = true
		walkRPF(t, dr, key.S, e.StreamOifs, e.TrafficSeg)
	}
	return e
}

// 路由器实际的流有效出接口
func actualStreamOifs(r *Router, key SG) map[string]bool {
	out := map[string]bool{}
	if g := r.G[key.G]; g != nil {
		for if_ := range r.gOifs(g, key.S) {
			out[if_.Name()] = true
		}
	}
	if st := r.SGm[key]; st != nil {
		for if_ := range r.sgOifs(st) {
			out[if_.Name()] = true
		}
	}
	return out
}

func actualRPTOifs(r *Router, g string) map[string]bool {
	out := map[string]bool{}
	if st := r.G[g]; st != nil {
		for if_ := range r.gStructuralOifs(st) {
			out[if_.Name()] = true
		}
	}
	return out
}

func fmtSet(m map[string]bool) string {
	if len(m) == 0 {
		return "∅"
	}
	var xs []string
	for x := range m {
		xs = append(xs, x)
	}
	sort.Strings(xs)
	return "{" + strings.Join(xs, ",") + "}"
}

// 对比每台路由器的出接口集合与期望值
func compareOifs(t *Topo, label string, expected map[string]map[string]bool, actual func(r *Router) map[string]bool, errs *[]string) {
	ids := make([]string, 0, len(t.Routers))
	for id := range t.Routers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := t.Routers[id]
		exp := expected[id]
		act := actual(r)
		if exp == nil {
			exp = map[string]bool{}
		}
		if fmtSet(exp) != fmtSet(act) {
			*errs = append(*errs, fmt.Sprintf("%s: 路由器 %s 出接口不符 期望%s 实际%s",
				label, id, fmtSet(exp), fmtSet(act)))
		}
	}
}

// 流量检查:窗口前后各网段该流的传输增量,必须恰好等于期望网段集合
func snapshotTx(t *Topo, key SG) map[string]int {
	snap := map[string]int{}
	for _, seg := range t.Segs {
		snap[seg.Name] = seg.TxCount[key]
	}
	return snap
}

func checkTraffic(t *Topo, key SG, snap map[string]int, expect map[string]bool, errs *[]string) {
	for _, seg := range t.Segs {
		delta := seg.TxCount[key] - snap[seg.Name]
		got := delta > 0
		want := expect[seg.Name]
		if got && !want {
			*errs = append(*errs, fmt.Sprintf("流量检查: 网段 %s 上不该有 %s 的流量(窗口内 %d 次)",
				seg.Name, key.S+","+key.G, delta))
		}
		if want && !got {
			*errs = append(*errs, fmt.Sprintf("流量检查: 网段 %s 上缺少 %s 的流量",
				seg.Name, key.S+","+key.G))
		}
	}
}

// 接收者统计汇总。maxSeq 传源已发序号;刚发出不到 500ms 的最后一包可能还在链路上,不计。
func receiverStats(t *Topo, key SG) (lines []string, totalLost, totalDup int) {
	src := t.Sources[key.S]
	maxSeq := 0
	if src != nil {
		maxSeq = src.Seq
		if t.Sim.Now-src.LastSent < 500 {
			maxSeq--
		}
	}
	ids := make([]string, 0, len(t.Hosts))
	for id := range t.Hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		h := t.Hosts[id]
		if !h.joined[key.G] {
			continue
		}
		recv, lost, dup := h.Stats(key, maxSeq)
		totalLost += lost
		totalDup += dup
		lines = append(lines, fmt.Sprintf("  %s: 收%d 丢%d 重%d", id, recv, lost, dup))
	}
	return
}
