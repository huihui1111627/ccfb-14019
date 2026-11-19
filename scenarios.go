package main

import (
	"fmt"
	"sort"
)

// 验收场景。每个场景结束后:
//  - 对比每台路由器的出接口表 vs 按拓扑最短路算出的应有的树
//  - 统计每个接收者丢包/重复
//  - 校验没人收的链路上没有这路流量

type Report struct {
	Name   string
	Errors []string
	Lines  []string
}

func (r *Report) errf(format string, a ...interface{}) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}
func (r *Report) logf(format string, a ...interface{}) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, a...))
}

func settle(sim *Sim, ms int64) { sim.RunUntil(sim.Now + ms) }

func setup() (*Sim, *Topo) {
	sim := &Sim{}
	t := BuildTopo(sim)
	for _, r := range t.Routers {
		r.Start()
	}
	settle(sim, 5000) // Hello 收敛,DR 选完
	return sim, t
}

func joinedHosts(t *Topo, g string) []*Host {
	var out []*Host
	ids := make([]string, 0, len(t.Hosts))
	for id := range t.Hosts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if t.Hosts[id].joined[g] {
			out = append(out, t.Hosts[id])
		}
	}
	return out
}

// 对比 (*,G) 共享树结构
func (rep *Report) verifyRPT(t *Topo, g string) {
	exp := ExpectRPT(t, g, joinedHosts(t, g))
	compareOifs(t, "(*,"+g+")共享树", exp.RPTOifs, func(r *Router) map[string]bool {
		return actualRPTOifs(r, g)
	}, &rep.Errors)
}

// 对比 (S,G) 流的实际出接口 + 流量网段检查(开窗统计 3s)
func (rep *Report) verifyStream(t *Topo, sim *Sim, key SG, switched bool) {
	exp := ExpectStream(t, key, joinedHosts(t, key.G), switched)
	compareOifs(t, fmt.Sprintf("(%s,%s)流", key.S, key.G), exp.StreamOifs, func(r *Router) map[string]bool {
		return actualStreamOifs(r, key)
	}, &rep.Errors)
	snap := snapshotTx(t, key)
	settle(sim, 3000)
	checkTraffic(t, key, snap, exp.TrafficSeg, &rep.Errors)
}

func (rep *Report) reportStats(t *Topo, key SG) {
	lines, lost, dup := receiverStats(t, key)
	for _, l := range lines {
		rep.logf("%s", l)
	}
	rep.logf("  合计: 丢%d 重%d", lost, dup)
}

func totalLossDup(t *Topo, key SG) (lost, dup int) {
	_, l, d := receiverStats(t, key)
	return l, d
}

// ---------- 场景 1:接收者加入/离开,共享树随之生长/修剪 ----------

func ScenarioJoinLeave() *Report {
	rep := &Report{Name: "join-leave 接收者加入离开"}
	sim, t := setup()

	t.Hosts["H4"].Join("G1")
	t.Hosts["H6"].Join("G1")
	t.Hosts["H7"].Join("G1")
	t.Hosts["H8"].Join("G1")
	t.Hosts["H10"].Join("G1")
	settle(sim, 3000)
	rep.verifyRPT(t, "G1")
	// lanM 上只有 DR(R8)可以替 H10 发 Join,R5 不许有 (*,G1) 状态
	if t.Routers["R5"].G["G1"] != nil {
		rep.errf("DR检查: R5 不是 lanM 的 DR,却建了 (*,G1) 状态")
	}

	t.Hosts["H8"].Leave("G1")
	t.Hosts["H10"].Leave("G1")
	settle(sim, 3000)
	rep.verifyRPT(t, "G1")
	if t.Routers["R8"].G["G1"] != nil {
		rep.errf("H8/H10 离开后 R8 仍保留 (*,G1) 状态")
	}

	t.Hosts["H4"].Leave("G1")
	t.Hosts["H6"].Leave("G1")
	t.Hosts["H7"].Leave("G1")
	settle(sim, 3000)
	for _, r := range t.Routers {
		if r.G["G1"] != nil {
			rep.errf("全部离开后 %s 仍保留 (*,G1) 状态", r.ID)
		}
	}
	rep.logf("共享树随加入/离开正确生长和拆除")
	return rep
}

// ---------- 场景 2:源启停,Register/Register-Stop,状态超时拆除 ----------

func ScenarioSourceStartStop() *Report {
	rep := &Report{Name: "source-start-stop 源启停"}
	sim, t := setup()
	t.SwitchThreshold = 10000 // 先关掉切换,观察 Register + 共享树阶段

	t.Hosts["H4"].Join("G1")
	t.Hosts["H6"].Join("G1")
	settle(sim, 3000)

	t.Sources["S1"].Start()
	settle(sim, 15000)

	key := SG{"S1", "G1"}
	rep.verifyStream(t, sim, key, false)
	if t.Routers["R1"].regStop[key] <= sim.Now {
		rep.errf("R1 未收到/未维持 Register-Stop")
	} else {
		rep.logf("R1 注册已被 RP 压停(Register-Stop 生效)")
	}
	rep.reportStats(t, key)

	t.Sources["S1"].Stop()
	settle(sim, 40000)
	for _, r := range t.Routers {
		if r.SGm[key] != nil {
			rep.errf("源停止后 %s 仍保留 (S1,G1) 状态", r.ID)
		}
	}
	// 源停了但接收者还在,(*,G1) 必须保留
	if t.Routers["R2"].G["G1"] == nil || t.Routers["R4"].G["G1"] == nil {
		rep.errf("源停止后 (*,G1) 共享树状态被误拆")
	}
	snap := snapshotTx(t, key)
	settle(sim, 3000)
	checkTraffic(t, key, snap, map[string]bool{}, &rep.Errors)
	rep.logf("源停止后 (S,G) 状态全部超时拆除,共享树保留")
	return rep
}

// ---------- 场景 3:SPT 切换,不丢不重,无人接收的链路无流量 ----------

func ScenarioSPTSwitch() *Report {
	rep := &Report{Name: "spt-switch 超门限切最短路径树"}
	sim, t := setup()
	for _, h := range []string{"H4", "H6", "H7", "H8", "H10"} {
		t.Hosts[h].Join("G1")
	}
	settle(sim, 3000)
	t.Sources["S1"].Start()
	settle(sim, 30000)

	key := SG{"S1", "G1"}
	// 每个最后一跳都应已切到 SPT
	for _, id := range []string{"R4", "R6", "R7", "R8"} {
		st := t.Routers[id].SGm[key]
		if st == nil || !st.SPTbit {
			rep.errf("最后一跳 %s 未完成 SPT 切换", id)
		}
	}
	rep.verifyRPT(t, "G1")              // 共享树结构仍在
	rep.verifyStream(t, sim, key, true) // 流量只走 SPT
	rep.reportStats(t, key)
	lost, dup := totalLossDup(t, key)
	if lost != 0 {
		rep.errf("切换过程丢包 %d 个", lost)
	}
	if dup != 0 {
		rep.errf("切换过程重复收包 %d 个", dup)
	}
	rep.logf("SPT 切换全程 0 丢包 0 重复;c34/c45/c24 等无人接收链路无 (S1,G1) 流量")
	return rep
}

// ---------- 场景 4:断链路,单播变,组播树跟着改道 ----------

func ScenarioLinkFail() *Report {
	rep := &Report{Name: "link-fail 断链改道"}
	sim, t := setup()
	for _, h := range []string{"H4", "H6", "H7", "H8", "H10"} {
		t.Hosts[h].Join("G1")
	}
	settle(sim, 3000)
	t.Sources["S1"].Start()
	settle(sim, 30000)
	key := SG{"S1", "G1"}

	t.SetSegUp("c23", false) // 断开 R2-R3
	rep.logf("c23(R2-R3)断开,等待重收敛...")
	settle(sim, 40000)
	rep.verifyRPT(t, "G1")
	rep.verifyStream(t, sim, key, true)
	lost1, dup1 := totalLossDup(t, key)
	rep.reportStats(t, key)
	rep.logf("断链重收敛期间丢 %d 包,重复 %d 包", lost1, dup1)
	if dup1 != 0 {
		rep.errf("改道过程出现重复包 %d 个", dup1)
	}

	t.SetSegUp("c23", true) // 恢复
	rep.logf("c23 恢复,等待切回...")
	settle(sim, 40000)
	rep.verifyRPT(t, "G1")
	rep.verifyStream(t, sim, key, true)
	return rep
}

// ---------- 场景 5:多接入网段 Assert 选出唯一转发者 ----------

func ScenarioAssert() *Report {
	rep := &Report{Name: "assert-lan 多接入网段断言"}
	sim, t := setup()
	// H8 在 R8 后、H10 直接挂在 lanM 上;R8 切换 SPT 后 R4/R5 都会往 lanM 转 (S1,G1),
	// 必须靠 Assert(+RPT 剪枝)收敛成唯一转发者
	t.Hosts["H8"].Join("G1")
	t.Hosts["H10"].Join("G1")
	settle(sim, 3000)
	t.Sources["S1"].Start()
	settle(sim, 30000)
	key := SG{"S1", "G1"}

	totalSent := 0
	for _, r := range t.Routers {
		totalSent += r.AssertsSent
	}
	if totalSent == 0 {
		rep.errf("lanM 上未发生 Assert 过程")
	} else {
		rep.logf("lanM 上共发出 %d 次 Assert,R4 输 %d 次 / R5 输 %d 次",
			totalSent, t.Routers["R4"].AssertsLost, t.Routers["R5"].AssertsLost)
	}

	// 开窗:稳态下 lanM 上该流只能有一个转发者
	snap := map[string]int{}
	var lanM *Seg
	for _, s := range t.Segs {
		if s.Name == "lanM" {
			lanM = s
		}
	}
	for k, v := range lanM.TxBy[key] {
		snap[k] = v
	}
	settle(sim, 3000)
	var forwarders []string
	for k, v := range lanM.TxBy[key] {
		if v-snap[k] > 0 {
			forwarders = append(forwarders, k)
		}
	}
	if len(forwarders) != 1 {
		rep.errf("稳态下 lanM 上 (S1,G1) 的转发者应为 1 个,实际 %v", forwarders)
	} else {
		rep.logf("稳态下 lanM 上唯一转发者: %s", forwarders[0])
	}
	rep.verifyStream(t, sim, key, true)
	rep.reportStats(t, key)
	lost, dup := totalLossDup(t, key)
	if lost != 0 || dup != 0 {
		rep.errf("Assert 场景接收者丢 %d 重 %d", lost, dup)
	}
	return rep
}

var Scenarios = []func() *Report{
	ScenarioJoinLeave,
	ScenarioSourceStartStop,
	ScenarioSPTSwitch,
	ScenarioLinkFail,
	ScenarioAssert,
}

func RunScenarios() ([]*Report, bool) {
	ok := true
	var reports []*Report
	for _, fn := range Scenarios {
		rep := fn()
		reports = append(reports, rep)
		if len(rep.Errors) > 0 {
			ok = false
		}
	}
	return reports, ok
}
