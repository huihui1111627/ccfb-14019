package main

// 单播路由:对路由器图跑全源 Dijkstra(拓扑变化时整体重算),
// 组播协议模块只通过 RPF() 查询到某个地址的反向路径接口/邻居/度量。
// 选路平局时按邻居路由器 ID 字典序取小,保证每台路由器和验收器算出同一棵树。

const MaxInt = 1 << 30

type Topo struct {
	Sim     *Sim
	Routers map[string]*Router
	Segs    []*Seg
	Hosts   map[string]*Host
	Sources map[string]*Source
	Dist    map[string]map[string]int // 路由器间最短距离
	owner   map[string]string         // 主机/源地址 -> 直连路由器 ID
	RP      string

	SwitchThreshold int               // SPT 切换门限(包数),场景可调
	Pos             map[string][2]int // 页面布局坐标
	SegPos          map[string][2]int
}

func (t *Topo) Recompute() {
	t.Dist = map[string]map[string]int{}
	for id := range t.Routers {
		t.Dist[id] = t.dijkstra(id)
	}
}

func (t *Topo) dijkstra(src string) map[string]int {
	dist := map[string]int{src: 0}
	done := map[string]bool{}
	for {
		u := ""
		best := MaxInt
		for id, d := range dist {
			if !done[id] && d < best {
				best, u = d, id
			}
		}
		if u == "" {
			break
		}
		done[u] = true
		for _, seg := range t.Segs {
			if !seg.Up {
				continue
			}
			has := false
			for _, if_ := range seg.Ifs {
				if if_.R.ID == u {
					has = true
					break
				}
			}
			if !has {
				continue
			}
			for _, if_ := range seg.Ifs {
				v := if_.R.ID
				if v == u {
					continue
				}
				nd := dist[u] + seg.Cost
				if cur, ok := dist[v]; !ok || nd < cur {
					dist[v] = nd
				}
			}
		}
	}
	return dist
}

type RPFInfo struct {
	If     *Iface // 出接口(朝向上游)
	Nbr    string // 上游邻居(路由器 ID;直连源时为主机 ID)
	Metric int
}

// RPF 计算 from 到 addr 的反向路径信息。addr 可以是路由器 ID、源或主机 ID。
func (t *Topo) RPF(from *Router, addr string) RPFInfo {
	owner := t.owner[addr]
	if owner == from.ID {
		if addr == from.ID {
			return RPFInfo{Nbr: from.ID, Metric: 0}
		}
		for _, seg := range t.Segs {
			for _, h := range seg.Hosts {
				if h.ID == addr {
					return RPFInfo{If: ifOnSeg(from, seg), Nbr: addr, Metric: seg.Cost}
				}
			}
			for _, s := range seg.Sources {
				if s.ID == addr {
					return RPFInfo{If: ifOnSeg(from, seg), Nbr: addr, Metric: seg.Cost}
				}
			}
		}
	}
	best := RPFInfo{Metric: MaxInt}
	for _, if_ := range from.Ifs {
		seg := if_.Seg
		if !seg.Up {
			continue
		}
		for _, oif := range seg.Ifs {
			if oif.R.ID == from.ID {
				continue
			}
			d, ok := t.Dist[oif.R.ID][owner]
			if !ok {
				continue
			}
			m := seg.Cost + d
			if m < best.Metric || (m == best.Metric && oif.R.ID < best.Nbr) {
				best = RPFInfo{If: if_, Nbr: oif.R.ID, Metric: m}
			}
		}
	}
	return best
}

func ifOnSeg(r *Router, seg *Seg) *Iface {
	for _, if_ := range r.Ifs {
		if if_.Seg == seg {
			return if_
		}
	}
	return nil
}

// SetSegUp 链路通断:重算单播,并通知每台路由器做组播 RPF 跟随
func (t *Topo) SetSegUp(name string, up bool) {
	for _, seg := range t.Segs {
		if seg.Name == name {
			seg.Up = up
		}
	}
	t.Recompute()
	for _, r := range t.Routers {
		r.onRouteChange()
	}
}

// SendUnicast 沿单播最短路径投递( Register / Register-Stop ),
// 时延按路径度量折算,不可达直接丢弃。
func (t *Topo) SendUnicast(from, to string, m Msg) {
	d, ok := t.Dist[from][to]
	if !ok {
		return
	}
	dst := t.Routers[to]
	t.Sim.After(int64(d)*2, func() { dst.onUnicast(m) })
}
