package main

import "sort"

// PIM-SM 稀疏模式路由器。
// 状态机要点:
//  - Hello 周期问候 + 网段 DR 选举(优先级,再比 ID)
//  - (*,G) 共享树:下游周期 Join 刷新,超时自动拆;离开/无下游立即 Prune
//  - 源直连 DR 把数据封装成 Register 单播给 RP;RP 沿 (S,G) 反向建树,
//    收到原生 (S,G) 数据后回 Register-Stop
//  - 最后一跳超过门限切 SPT,并在共享树上对该 (S,G) 发 RPT 剪枝;
//    切换期间两棵树同时收,靠路由器级去重保证不丢不重
//  - 同一网段多个转发者靠 Assert 选赢家(比到源/RP 的度量,再比 ID)
//  - 所有数据/加入报文做 RPF 检查;单播变化时全量刷新 RPF 并换道

const (
	HelloInt   int64 = 4000
	JPInt      int64 = 8000
	Expiry     int64 = 24000 // Join/Prune 保持时间 = 3x 刷新周期
	NbrHold    int64 = 15000
	Keepalive  int64 = 24000 // (S,G) 活跃保持(数据刷新)
	AssertHold int64 = 24000
)

type Nbr struct {
	Prio    int
	Expires int64
}

// (*,G) 共享树状态
type GState struct {
	G      string
	Iif    *Iface // RPF(RP) 接口
	UpNbr  string
	Joins  map[*Iface]int64            // 下游 Join,按接口记录超时时刻
	Pruned map[*Iface]map[string]int64 // 下游发来的 (S,G)RPT 剪枝:接口 -> S -> 超时
}

// (S,G) 最短路径树状态
type SGState struct {
	Key        SG
	Iif        *Iface // RPF(S) 接口
	UpNbr      string
	Joins      map[*Iface]int64
	LocalOifs  map[*Iface]bool  // 最后一跳切换后,本地接收者接口挂在这里
	Pending    map[*Iface]int64 // 多接入网段上断言探针未决的出接口
	SPTbit     bool             // 已从 SPT 接口收到原生数据
	AliveUntil int64
}

type Iface struct {
	R   *Router
	Seg *Seg
}

func (i *Iface) Name() string { return i.R.ID + "·" + i.Seg.Name }

type Router struct {
	ID   string
	Sim  *Sim
	Topo *Topo
	Prio int
	Ifs  []*Iface

	nbrs     map[*Iface]map[string]*Nbr
	localMem map[*Iface]map[string]bool // IGMP 直连接收者

	G   map[string]*GState
	SGm map[SG]*SGState

	assertLost  map[SG]map[*Iface]int64 // Assert 输掉的接口(按流)
	regStop     map[SG]int64            // 第一跳收到 Register-Stop
	rpRegSender map[SG]string           // RP 记录 Register 来源
	rateCnt     map[SG]int              // 切换门限计数
	seen        map[SG]map[int]bool     // 转发去重缓存

	Forwarded map[SG]int // 统计

	AssertsSent int
	AssertsLost int
}

func NewRouter(sim *Sim, t *Topo, id string) *Router {
	return &Router{
		ID: id, Sim: sim, Topo: t, Prio: 10,
		nbrs:        map[*Iface]map[string]*Nbr{},
		localMem:    map[*Iface]map[string]bool{},
		G:           map[string]*GState{},
		SGm:         map[SG]*SGState{},
		assertLost:  map[SG]map[*Iface]int64{},
		regStop:     map[SG]int64{},
		rpRegSender: map[SG]string{},
		rateCnt:     map[SG]int{},
		seen:        map[SG]map[int]bool{},
		Forwarded:   map[SG]int{},
	}
}

func (r *Router) Start() {
	r.helloLoop()
	r.Sim.After(1000, r.jpLoop)
	r.Sim.After(2000, r.reaperLoop)
}

// ---------- Hello 与 DR 选举 ----------

func (r *Router) helloLoop() {
	for _, if_ := range r.Ifs {
		if_.Seg.transmitMsg(if_, "", Msg{Kind: MHello, From: r.ID, Prio: r.Prio})
	}
	r.Sim.After(HelloInt, r.helloLoop)
}

func (r *Router) IsDR(if_ *Iface) bool {
	for id, n := range r.nbrs[if_] {
		if n.Prio > r.Prio || (n.Prio == r.Prio && id > r.ID) {
			return false
		}
	}
	return true
}

// ---------- 报文入口 ----------

func (r *Router) onMsg(if_ *Iface, m Msg) {
	now := r.Sim.Now
	switch m.Kind {
	case MHello:
		if r.nbrs[if_] == nil {
			r.nbrs[if_] = map[string]*Nbr{}
		}
		r.nbrs[if_][m.From] = &Nbr{Prio: m.Prio, Expires: now + NbrHold}
	case MReport:
		if r.localMem[if_] == nil {
			r.localMem[if_] = map[string]bool{}
		}
		r.localMem[if_][m.G] = true
		if r.IsDR(if_) {
			r.getG(m.G) // DR 替本网段接收者往上游加入
		}
	case MLeave:
		if r.localMem[if_] != nil {
			delete(r.localMem[if_], m.G)
		}
		if g := r.G[m.G]; g != nil && !r.gNeedsUpstream(g) {
			r.pruneUpG(g)
		}
		for key, st := range r.SGm {
			if key.G == m.G {
				delete(st.LocalOifs, if_)
				r.maybePruneUpSG(st)
			}
		}
	case MJoinPrune:
		if m.Target != r.ID {
			return // 多接入网段上只有被点名的上游处理
		}
		r.onJoinPrune(if_, m)
	case MAssert:
		r.onAssert(if_, m)
	}
}

func (r *Router) onUnicast(m Msg) {
	switch m.Kind {
	case MRegister:
		r.onRegister(m)
	case MRegStop:
		r.regStop[m.SGKey] = r.Sim.Now + Expiry
	}
}

func (r *Router) onJoinPrune(if_ *Iface, m Msg) {
	now := r.Sim.Now
	for _, sg := range m.Joins {
		if sg.S == "" {
			r.getG(m.G).Joins[if_] = now + Expiry
		} else {
			st := r.getSG(sg)
			// 多接入网段上新增 (S,G) 出接口:先发 Assert 探针、延迟开闸,
			// 让网段上现有转发者先退出,避免同一流两台一起转
			if _, ok := st.Joins[if_]; !ok && len(if_.Seg.Ifs) > 2 && st.Iif != if_ {
				if st.Pending == nil {
					st.Pending = map[*Iface]int64{}
				}
				st.Pending[if_] = now + 3*if_.Seg.Delay
				r.sendAssert(if_, sg)
			}
			st.Joins[if_] = now + Expiry
			st.AliveUntil = now + Keepalive
		}
	}
	for _, sg := range m.Prunes {
		if sg.S == "" {
			if g := r.G[m.G]; g != nil {
				delete(g.Joins, if_)
				delete(g.Pruned, if_)
				r.maybePruneUpG(g)
			}
		} else {
			if st := r.SGm[sg]; st != nil {
				delete(st.Joins, if_)
				r.maybePruneUpSG(st)
			}
		}
	}
	for _, sg := range m.PruneRPT {
		if g := r.G[sg.G]; g != nil {
			if g.Pruned[if_] == nil {
				g.Pruned[if_] = map[string]int64{}
			}
			g.Pruned[if_][sg.S] = now + Expiry
		}
	}
}

// ---------- 状态创建 ----------

func (r *Router) getG(gname string) *GState {
	if g := r.G[gname]; g != nil {
		return g
	}
	g := &GState{G: gname, Joins: map[*Iface]int64{}, Pruned: map[*Iface]map[string]int64{}}
	r.G[gname] = g
	if r.ID != r.Topo.RP {
		ri := r.Topo.RPF(r, r.Topo.RP)
		g.Iif, g.UpNbr = ri.If, ri.Nbr
		r.sendJoinG(g) // 立即往 RP 方向发 Join,加速建树
	}
	return g
}

func (r *Router) getSG(key SG) *SGState {
	if st := r.SGm[key]; st != nil {
		return st
	}
	st := &SGState{Key: key, Joins: map[*Iface]int64{}, LocalOifs: map[*Iface]bool{}}
	r.SGm[key] = st
	if r.Topo.owner[key.S] != r.ID {
		ri := r.Topo.RPF(r, key.S)
		st.Iif, st.UpNbr = ri.If, ri.Nbr
		r.sendJoinSG(st) // 立即往源方向发 (S,G) Join
	} else {
		st.Iif = r.Topo.RPF(r, key.S).If // 第一跳:朝向源的接口
	}
	return st
}

// ---------- Join/Prune 发送 ----------

func (r *Router) sendJP(if_ *Iface, target, g string, joins, prunes, prunesRPT []SG) {
	if if_ == nil || !if_.Seg.Up {
		return
	}
	if_.Seg.transmitMsg(if_, "", Msg{
		Kind: MJoinPrune, From: r.ID, Target: target, G: g,
		Joins: joins, Prunes: prunes, PruneRPT: prunesRPT,
	})
}

func (r *Router) sendJoinG(g *GState) {
	if g.Iif == nil || r.ID == r.Topo.RP {
		return
	}
	r.sendJP(g.Iif, g.UpNbr, g.G, []SG{{"", g.G}}, nil, r.rptPruneSet(g))
}

func (r *Router) sendJoinSG(st *SGState) {
	if st.Iif == nil || r.Topo.owner[st.Key.S] == r.ID {
		return
	}
	r.sendJP(st.Iif, st.UpNbr, st.Key.G, []SG{st.Key}, nil, nil)
}

func (r *Router) pruneUpG(g *GState) {
	if r.ID != r.Topo.RP && g.Iif != nil {
		r.sendJP(g.Iif, g.UpNbr, g.G, nil, []SG{{"", g.G}}, nil)
	}
	delete(r.G, g.G)
}

func (r *Router) maybePruneUpG(g *GState) {
	if !r.gNeedsUpstream(g) {
		r.pruneUpG(g)
	}
}

// (S,G) 是否还有下游要这条流
func (r *Router) sgHasDownstream(st *SGState) bool {
	if len(st.LocalOifs) > 0 || r.hasJoins(st.Joins) {
		return true
	}
	// RP:共享树上还有这条流的下游,就得保住 (S,G) 状态
	if r.ID == r.Topo.RP {
		if g := r.G[st.Key.G]; g != nil && len(r.gOifs(g, st.Key.S)) > 0 {
			return true
		}
	}
	return false
}

func (r *Router) maybePruneUpSG(st *SGState) {
	if r.sgHasDownstream(st) {
		return
	}
	if st.Iif != nil && r.Topo.owner[st.Key.S] != r.ID {
		r.sendJP(st.Iif, st.UpNbr, st.Key.G, nil, []SG{st.Key}, nil)
	}
	delete(r.SGm, st.Key)
}

func (r *Router) hasJoins(joins map[*Iface]int64) bool {
	now := r.Sim.Now
	for _, exp := range joins {
		if exp > now {
			return true
		}
	}
	return false
}

func (r *Router) gNeedsUpstream(g *GState) bool {
	if r.hasJoins(g.Joins) {
		return true
	}
	for if_, groups := range r.localMem {
		if groups[g.G] && r.IsDR(if_) {
			return true
		}
	}
	return false
}

func (r *Router) sgNeedsUpstream(st *SGState) bool {
	if st.AliveUntil <= r.Sim.Now {
		return false
	}
	return r.sgHasDownstream(st)
}

// rptPruneSet 需要在共享树上单独剪掉的源:
// 本机已切到 SPT(SPTbit 且 SPT 有下游),或该流在本机的共享树出接口已全部被剪/Assert 输掉。
func (r *Router) rptPruneSet(g *GState) []SG {
	cand := map[string]bool{}
	for key := range r.SGm {
		if key.G == g.G {
			cand[key.S] = true
		}
	}
	for _, m := range g.Pruned {
		for s := range m {
			cand[s] = true
		}
	}
	for key := range r.assertLost {
		if key.G == g.G {
			cand[key.S] = true
		}
	}
	var out []SG
	for s := range cand {
		key := SG{s, g.G}
		st := r.SGm[key]
		switched := st != nil && st.SPTbit && len(r.sgOifs(st)) > 0
		if switched || len(r.gOifs(g, s)) == 0 {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].S < out[j].S })
	return out
}

// ---------- 出接口集合 ----------

func (r *Router) prunedOn(g *GState, if_ *Iface, s string) bool {
	return g.Pruned[if_][s] > r.Sim.Now
}

func (r *Router) assertLostOn(key SG, if_ *Iface) bool {
	return r.assertLost[key][if_] > r.Sim.Now
}

// (*,G) 对某条流的有效出接口(剪掉 RPT-pruned 和 Assert 输掉的)
func (r *Router) gOifs(g *GState, s string) map[*Iface]bool {
	now := r.Sim.Now
	out := map[*Iface]bool{}
	key := SG{s, g.G}
	add := func(if_ *Iface) {
		if r.prunedOn(g, if_, s) || r.assertLostOn(key, if_) {
			return
		}
		out[if_] = true
	}
	for if_, exp := range g.Joins {
		if exp > now {
			add(if_)
		}
	}
	for if_, groups := range r.localMem {
		if groups[g.G] && r.IsDR(if_) {
			add(if_)
		}
	}
	return out
}

// (*,G) 结构出接口(不做按流过滤),供验收器对照共享树形状
func (r *Router) gStructuralOifs(g *GState) map[*Iface]bool {
	now := r.Sim.Now
	out := map[*Iface]bool{}
	for if_, exp := range g.Joins {
		if exp > now {
			out[if_] = true
		}
	}
	for if_, groups := range r.localMem {
		if groups[g.G] && r.IsDR(if_) {
			out[if_] = true
		}
	}
	return out
}

func (r *Router) sgOifs(st *SGState) map[*Iface]bool {
	now := r.Sim.Now
	out := map[*Iface]bool{}
	for if_, exp := range st.Joins {
		if exp > now && !r.assertLostOn(st.Key, if_) && st.Pending[if_] <= now {
			out[if_] = true
		}
	}
	for if_ := range st.LocalOifs {
		if !r.assertLostOn(st.Key, if_) {
			out[if_] = true
		}
	}
	return out
}

func (r *Router) isOif(if_ *Iface, key SG) bool {
	if g := r.G[key.G]; g != nil {
		if r.gOifs(g, key.S)[if_] {
			return true
		}
	}
	if st := r.SGm[key]; st != nil {
		if r.sgOifs(st)[if_] {
			return true
		}
	}
	return false
}

// ---------- 数据转发 ----------

func (r *Router) onData(if_ *Iface, d DataPkt) {
	key := SG{d.S, d.G}
	now := r.Sim.Now

	// 第一跳 DR:直连源,负责 Register 封装
	if r.Topo.owner[d.S] == r.ID && r.IsDR(if_) {
		r.firstHopData(d)
	}

	matched := false
	oifs := map[*Iface]bool{}
	if st := r.SGm[key]; st != nil && st.Iif == if_ {
		matched = true
		st.AliveUntil = now + Keepalive
		if !st.SPTbit {
			st.SPTbit = true
			r.onSPTbit(st)
		}
		for o := range r.sgOifs(st) {
			oifs[o] = true
		}
		// (S,G) 继承 (*,G) 的出接口(RP 上尤其关键:共享树下游都挂在这)
		if g := r.G[d.G]; g != nil {
			for o := range r.gOifs(g, d.S) {
				oifs[o] = true
			}
		}
	}
	if g := r.G[d.G]; g != nil && g.Iif == if_ {
		matched = true
		for o := range r.gOifs(g, d.S) {
			oifs[o] = true
		}
	}
	if !matched {
		// RPF 检查失败;若报文落在自己的出接口上,说明网段上有别的转发者 → Assert
		if r.isOif(if_, key) {
			r.sendAssert(if_, key)
		}
		return
	}
	delete(oifs, if_)
	// 切换过渡期两棵树可能同时送同一份数据,路由器级去重保证下游不重
	if r.seenAlready(key, d.Seq) {
		return
	}
	r.markSeen(key, d.Seq)

	// 最后一跳:统计速率,超门限切 SPT
	if st := r.SGm[key]; (st == nil || len(st.LocalOifs) == 0) && r.localDRMember(d.G) {
		r.rateCnt[key]++
		if r.rateCnt[key] >= r.Topo.SwitchThreshold {
			r.startSwitch(key)
		}
	}
	for o := range oifs {
		o.Seg.transmitData(o, "", d)
		r.Forwarded[key]++
	}
}

func (r *Router) localDRMember(g string) bool {
	for if_, groups := range r.localMem {
		if groups[g] && r.IsDR(if_) {
			return true
		}
	}
	return false
}

// 第一跳:把源数据封装成 Register 单播给 RP
func (r *Router) firstHopData(d DataPkt) {
	key := SG{d.S, d.G}
	if r.regStop[key] > r.Sim.Now {
		return
	}
	r.Topo.SendUnicast(r.ID, r.Topo.RP, Msg{Kind: MRegister, From: r.ID, Data: d})
}

// RP 收 Register:有接收者就解封转发并沿 (S,G) 反向建树,否则回 Register-Stop
func (r *Router) onRegister(m Msg) {
	d := m.Data
	key := SG{d.S, d.G}
	// SPT 已建好后还收到 Register,说明对端 Register-Stop 超时重发了,直接再压掉
	if st := r.SGm[key]; st != nil && st.SPTbit {
		r.Topo.SendUnicast(r.ID, m.From, Msg{Kind: MRegStop, SGKey: key})
		return
	}
	g := r.G[d.G]
	var oifs map[*Iface]bool
	if g != nil {
		oifs = r.gOifs(g, d.S)
	}
	if len(oifs) == 0 {
		r.Topo.SendUnicast(r.ID, m.From, Msg{Kind: MRegStop, SGKey: key})
		return
	}
	r.rpRegSender[key] = m.From
	if !r.seenAlready(key, d.Seq) {
		r.markSeen(key, d.Seq)
		for o := range oifs {
			o.Seg.transmitData(o, "", d)
		}
	}
	st := r.getSG(key)
	st.AliveUntil = r.Sim.Now + Keepalive
}

// SPTbit 置位:RP 回 Register-Stop;非 RP 在共享树上剪掉这个 (S,G)
func (r *Router) onSPTbit(st *SGState) {
	if r.ID == r.Topo.RP {
		if sender, ok := r.rpRegSender[st.Key]; ok {
			r.Topo.SendUnicast(r.ID, sender, Msg{Kind: MRegStop, SGKey: st.Key})
		}
		return
	}
	if g := r.G[st.Key.G]; g != nil && g.Iif != nil {
		r.sendJP(g.Iif, g.UpNbr, g.G, []SG{{"", g.G}}, nil, []SG{st.Key})
	}
}

// 最后一跳发起 SPT 切换:往源方向发 (S,G) Join,本地接收者挂到 (S,G) 下。
// 共享树出接口保留到 SPT 数据真正到达(见 onData 的 SPTbit 与去重),保证不丢不重。
func (r *Router) startSwitch(key SG) {
	st := r.getSG(key)
	st.AliveUntil = r.Sim.Now + Keepalive
	for if_, groups := range r.localMem {
		if groups[key.G] && r.IsDR(if_) {
			st.LocalOifs[if_] = true
		}
	}
}

// ---------- Assert ----------

// 度量:(是否已有 (S,G) 状态的偏好, 度量值);有 (S,G) 者比到源,否则比到 RP
func (r *Router) assertMetric(key SG) (int, int) {
	if r.SGm[key] != nil {
		return 0, r.Topo.RPF(r, key.S).Metric
	}
	return 1, r.Topo.RPF(r, r.Topo.RP).Metric
}

func assertWins(pref, met int, id string, opref, omet int, oid string) bool {
	if pref != opref {
		return pref < opref
	}
	if met != omet {
		return met < omet
	}
	return id > oid // 平局比大 ID
}

func (r *Router) sendAssert(if_ *Iface, key SG) {
	pref, met := r.assertMetric(key)
	r.AssertsSent++
	if_.Seg.transmitMsg(if_, "", Msg{
		Kind: MAssert, From: r.ID, SGKey: key, MetricPref: pref, Metric: met,
	})
}

func (r *Router) onAssert(if_ *Iface, m Msg) {
	// 该接口是这条流的 RPF 入接口时,本网段对我们而言是"上游",
	// 不参与转发者竞争(数据本来就从这里来)
	if st := r.SGm[m.SGKey]; st != nil && st.Iif == if_ {
		return
	}
	if g := r.G[m.SGKey.G]; g != nil && g.Iif == if_ {
		return
	}
	if !r.isOif(if_, m.SGKey) {
		return // 我们不在往这个网段转这条流,不参与竞争
	}
	pref, met := r.assertMetric(m.SGKey)
	if assertWins(pref, met, r.ID, m.MetricPref, m.Metric, m.From) {
		return // 我赢,继续转发
	}
	if r.assertLost[m.SGKey] == nil {
		r.assertLost[m.SGKey] = map[*Iface]int64{}
	}
	r.assertLost[m.SGKey][if_] = r.Sim.Now + AssertHold
	r.AssertsLost++
	// 输家若共享树上该流已无出接口,立即往 RP 方向补 RPT 剪枝
	if g := r.G[m.SGKey.G]; g != nil && g.Iif != nil && r.ID != r.Topo.RP {
		r.sendJP(g.Iif, g.UpNbr, g.G, []SG{{"", g.G}}, nil, r.rptPruneSet(g))
	}
}

// ---------- 周期任务 ----------

func (r *Router) jpLoop() {
	r.refreshAll()
	// DR 变化后补建 (*,G)
	for if_, groups := range r.localMem {
		for g := range groups {
			if r.IsDR(if_) && r.G[g] == nil {
				r.getG(g)
			}
		}
	}
	for _, g := range r.sortedG() {
		if r.ID == r.Topo.RP || !r.gNeedsUpstream(g) {
			continue
		}
		r.sendJoinG(g)
	}
	for _, st := range r.sortedSG() {
		if r.sgNeedsUpstream(st) {
			r.sendJoinSG(st)
		}
	}
	r.Sim.After(JPInt, r.jpLoop)
}

func (r *Router) reaperLoop() {
	now := r.Sim.Now
	for _, ns := range r.nbrs {
		for id, n := range ns {
			if n.Expires <= now {
				delete(ns, id)
			}
		}
	}
	for _, g := range r.G {
		for if_, exp := range g.Joins {
			if exp <= now {
				delete(g.Joins, if_)
			}
		}
		for _, m := range g.Pruned {
			for s, exp := range m {
				if exp <= now {
					delete(m, s)
				}
			}
		}
	}
	for key, st := range r.SGm {
		for if_, exp := range st.Joins {
			if exp <= now {
				delete(st.Joins, if_)
			}
		}
		for if_, exp := range st.Pending {
			if exp <= now {
				delete(st.Pending, if_)
			}
		}
		// 数据流停了:Keepalive 超时,(S,G) 状态拆除并向上游剪枝
		if st.AliveUntil <= now {
			if r.sgHasDownstream(st) && st.Iif != nil && r.Topo.owner[st.Key.S] != r.ID {
				r.sendJP(st.Iif, st.UpNbr, st.Key.G, nil, []SG{st.Key}, nil)
			}
			delete(r.SGm, key)
		}
	}
	for key, m := range r.assertLost {
		for if_, exp := range m {
			if exp <= now {
				delete(m, if_)
			}
		}
		if len(m) == 0 {
			delete(r.assertLost, key)
		}
	}
	for key, exp := range r.regStop {
		if exp <= now {
			delete(r.regStop, key)
		}
	}
	// 共享树/源树下游全部消失(未发 Prune,如断链)→ 主动向上游剪枝
	for _, g := range r.sortedG() {
		if !r.gNeedsUpstream(g) {
			r.pruneUpG(g)
		}
	}
	for _, st := range r.sortedSG() {
		r.maybePruneUpSG(st)
	}
	r.Sim.After(2000, r.reaperLoop)
}

// ---------- 单播变化跟随 ----------

func (r *Router) onRouteChange() {
	for if_ := range r.nbrs {
		if !if_.Seg.Up {
			r.nbrs[if_] = map[string]*Nbr{}
		}
	}
	r.refreshAll()
}

func (r *Router) refreshAll() {
	for _, g := range r.G {
		r.refreshG(g)
	}
	for _, st := range r.SGm {
		r.refreshSG(st)
	}
}

// RPF 变了:旧邻居发 Prune,新邻居发 Join,组播树跟着单播改道
func (r *Router) refreshG(g *GState) {
	if r.ID == r.Topo.RP {
		return
	}
	ri := r.Topo.RPF(r, r.Topo.RP)
	if ri.If == g.Iif && ri.Nbr == g.UpNbr {
		return
	}
	oldIf, oldNbr := g.Iif, g.UpNbr
	g.Iif, g.UpNbr = ri.If, ri.Nbr
	if oldIf != nil && oldIf.Seg.Up && oldNbr != "" {
		r.sendJP(oldIf, oldNbr, g.G, nil, []SG{{"", g.G}}, nil)
	}
	if g.Iif != nil && r.gNeedsUpstream(g) {
		r.sendJoinG(g)
	}
}

func (r *Router) refreshSG(st *SGState) {
	if r.Topo.owner[st.Key.S] == r.ID {
		return
	}
	ri := r.Topo.RPF(r, st.Key.S)
	if ri.If == st.Iif && ri.Nbr == st.UpNbr {
		return
	}
	oldIf, oldNbr := st.Iif, st.UpNbr
	st.Iif, st.UpNbr = ri.If, ri.Nbr
	if oldIf != nil && oldIf.Seg.Up && oldNbr != "" {
		r.sendJP(oldIf, oldNbr, st.Key.G, nil, []SG{st.Key}, nil)
	}
	if st.Iif != nil && r.sgNeedsUpstream(st) {
		r.sendJoinSG(st)
	}
}

// ---------- 去重缓存 ----------

func (r *Router) seenAlready(key SG, seq int) bool {
	return r.seen[key][seq]
}

func (r *Router) markSeen(key SG, seq int) {
	m := r.seen[key]
	if m == nil {
		m = map[int]bool{}
		r.seen[key] = m
	}
	m[seq] = true
	if len(m) > 400 {
		max := 0
		for s := range m {
			if s > max {
				max = s
			}
		}
		for s := range m {
			if s < max-200 {
				delete(m, s)
			}
		}
	}
}

// ---------- 排序辅助(确定性) ----------

func (r *Router) sortedG() []*GState {
	names := make([]string, 0, len(r.G))
	for n := range r.G {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*GState, 0, len(names))
	for _, n := range names {
		out = append(out, r.G[n])
	}
	return out
}

func (r *Router) sortedSG() []*SGState {
	keys := make([]SG, 0, len(r.SGm))
	for k := range r.SGm {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].G != keys[j].G {
			return keys[i].G < keys[j].G
		}
		return keys[i].S < keys[j].S
	})
	out := make([]*SGState, 0, len(keys))
	for _, k := range keys {
		out = append(out, r.SGm[k])
	}
	return out
}
