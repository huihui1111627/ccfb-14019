package main

// 仿真拓扑:省台源 S1 在 R1 后,RP 在 R2。
// R1-R2-R3-R4-R5-R1 成环,R2-R4 为斜拉链路(多路径);
// lanM(R4/R5/R8 + 主机 H10)和 lanA(R3/R6/R7)是多接入网段,用于 DR 选举与 Assert。
//
//	S1                 H6   H7
//	|                   |    |
//	R1 --c12- R2 -c23- R3   R6   R7
//	|         |\        |     \  /
//	   c51    | \c24  c34     lanA
//	  /       |  \      |
//	R5 -c45- R4 -+      |
//	 \       / |        |
//	  \-lanM-/ lanB     |
//	   |  \   H4        |
//	  R8  H10           |
//	  |
//	lanN
//	  |
//	 H8
//
// 网段:sSrc:S1-R1(1)  c12:R1-R2(10)  c23:R2-R3(10)  c34:R3-R4(10)
//
//	c45:R4-R5(10)  c51:R5-R1(10)  c24:R2-R4(25)
//	lanM:R4/R5/R8+H10(5)  lanA:R3/R6/R7(5)
//	lanN:R8-H8(1)  lanB:R4-H4(1)  lanD:R6-H6(1)  lanE:R7-H7(1)
func BuildTopo(sim *Sim) *Topo {
	t := &Topo{
		Sim:             sim,
		Routers:         map[string]*Router{},
		Hosts:           map[string]*Host{},
		Sources:         map[string]*Source{},
		owner:           map[string]string{},
		RP:              "R2",
		SwitchThreshold: 8,
		Pos:             map[string][2]int{},
		SegPos:          map[string][2]int{},
	}
	mkRouter := func(id string, x, y int) *Router {
		r := NewRouter(sim, t, id)
		t.Routers[id] = r
		t.owner[id] = id
		t.Pos[id] = [2]int{x, y}
		return r
	}
	mkSeg := func(name string, cost int, delay int64, x, y int) *Seg {
		s := &Seg{
			Name: name, Cost: cost, Delay: delay, Up: true,
			Sim: sim, Topo: t,
			TxCount: map[SG]int{}, TxBy: map[SG]map[string]int{},
		}
		t.Segs = append(t.Segs, s)
		t.SegPos[name] = [2]int{x, y}
		return s
	}
	attach := func(r *Router, seg *Seg) {
		if_ := &Iface{R: r, Seg: seg}
		r.Ifs = append(r.Ifs, if_)
		seg.Ifs = append(seg.Ifs, if_)
	}
	mkHost := func(id string, seg *Seg, x, y int) *Host {
		h := NewHost(sim, id)
		h.Seg = seg
		seg.Hosts = append(seg.Hosts, h)
		t.Hosts[id] = h
		t.Pos[id] = [2]int{x, y}
		return h
	}

	R1 := mkRouter("R1", 170, 60)
	R2 := mkRouter("R2", 320, 50)
	R3 := mkRouter("R3", 470, 60)
	R4 := mkRouter("R4", 470, 200)
	R5 := mkRouter("R5", 300, 220)
	R6 := mkRouter("R6", 610, 130)
	R7 := mkRouter("R7", 610, 210)
	R8 := mkRouter("R8", 470, 330)

	sSrc := mkSeg("sSrc", 1, 5, 90, 60)
	c12 := mkSeg("c12", 10, 5, 245, 55)
	c23 := mkSeg("c23", 10, 5, 395, 55)
	c34 := mkSeg("c34", 10, 5, 470, 130)
	c45 := mkSeg("c45", 10, 5, 385, 210)
	c51 := mkSeg("c51", 10, 5, 235, 140)
	c24 := mkSeg("c24", 25, 5, 395, 125)
	lanM := mkSeg("lanM", 5, 5, 385, 300)
	lanA := mkSeg("lanA", 5, 5, 545, 130)
	lanN := mkSeg("lanN", 1, 5, 520, 380)
	lanB := mkSeg("lanB", 1, 5, 545, 240)
	lanD := mkSeg("lanD", 1, 5, 680, 130)
	lanE := mkSeg("lanE", 1, 5, 680, 210)

	// 源 S1 挂在 R1 后
	src := &Source{ID: "S1", Seg: sSrc, Sim: sim, G: "G1", Interval: 1000}
	sSrc.Sources = append(sSrc.Sources, src)
	t.Sources["S1"] = src
	t.owner["S1"] = "R1"
	t.Pos["S1"] = [2]int{40, 60}

	attach(R1, sSrc)
	attach(R1, c12)
	attach(R1, c51)
	attach(R2, c12)
	attach(R2, c23)
	attach(R2, c24)
	attach(R3, c23)
	attach(R3, c34)
	attach(R3, lanA)
	attach(R4, c34)
	attach(R4, c45)
	attach(R4, c24)
	attach(R4, lanM)
	attach(R4, lanB)
	attach(R5, c45)
	attach(R5, c51)
	attach(R5, lanM)
	attach(R6, lanA)
	attach(R6, lanD)
	attach(R7, lanA)
	attach(R7, lanE)
	attach(R8, lanM)
	attach(R8, lanN)

	mkHost("H4", lanB, 600, 260)
	mkHost("H6", lanD, 740, 130)
	mkHost("H7", lanE, 740, 210)
	mkHost("H8", lanN, 560, 410)
	mkHost("H10", lanM, 330, 340)

	t.Recompute()
	return t
}
