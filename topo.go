package main

// 网络拓扑模型:网段(Segment)是唯一的传输介质。
// 点到点链路 = 只有两个路由器接口的网段;
// 多接入网段(lanM/lanA)= 多个路由器接口 + 可能挂主机,天然支持 DR 选举和 Assert。

type DataPkt struct {
	S, G string
	Seq  int
}

type SG struct{ S, G string }

type MsgKind int

const (
	MHello MsgKind = iota
	MJoinPrune
	MAssert
	MReport // IGMP 成员报告
	MLeave  // IGMP 离开
	MRegister
	MRegStop
)

type Msg struct {
	Kind MsgKind
	From string // 发送路由器 ID(或主机 ID)
	Prio int    // Hello 里的 DR 优先级

	// Join/Prune 载荷
	Target   string // 上游邻居(只有它处理这条消息)
	G        string
	Joins    []SG // S=="" 表示 (*,G)
	Prunes   []SG
	PruneRPT []SG // 从共享树上单独剪掉的 (S,G)

	// Assert 载荷
	SGKey      SG
	MetricPref int
	Metric     int

	// Register 载荷
	Data DataPkt
}

type Seg struct {
	Name    string
	Cost    int // 单播度量
	Delay   int64
	Up      bool
	Ifs     []*Iface
	Hosts   []*Host
	Sources []*Source
	Sim     *Sim
	Topo    *Topo

	// 统计:每条流在本网段上被转发的次数、分别是谁发的
	TxCount map[SG]int
	TxBy    map[SG]map[string]int
}

func (s *Seg) transmitData(fromIf *Iface, fromHostID string, d DataPkt) {
	key := SG{d.S, d.G}
	if s.TxCount == nil {
		s.TxCount = map[SG]int{}
		s.TxBy = map[SG]map[string]int{}
	}
	s.TxCount[key]++
	sender := fromHostID
	if fromIf != nil {
		sender = fromIf.R.ID
	}
	if s.TxBy[key] == nil {
		s.TxBy[key] = map[string]int{}
	}
	s.TxBy[key][sender]++
	if !s.Up {
		return
	}
	for _, if_ := range s.Ifs {
		if if_ == fromIf {
			continue
		}
		dst := if_
		s.Sim.After(s.Delay, func() { dst.R.onData(dst, d) })
	}
	for _, h := range s.Hosts {
		if h.ID == fromHostID {
			continue
		}
		hh := h
		s.Sim.After(s.Delay, func() { hh.onData(d) })
	}
}

func (s *Seg) transmitMsg(fromIf *Iface, fromHostID string, m Msg) {
	if !s.Up {
		return
	}
	for _, if_ := range s.Ifs {
		if if_ == fromIf {
			continue
		}
		dst := if_
		s.Sim.After(s.Delay, func() { dst.R.onMsg(dst, m) })
	}
}

// 主机:接收者(IGMP 加入/离开,统计丢包和重复)
type Host struct {
	ID     string
	Seg    *Seg
	Sim    *Sim
	joined map[string]bool
	recv   map[SG]map[int]bool
	dups   map[SG]int
	first  map[SG]int
}

func NewHost(sim *Sim, id string) *Host {
	return &Host{
		ID: id, Sim: sim,
		joined: map[string]bool{},
		recv:   map[SG]map[int]bool{},
		dups:   map[SG]int{},
		first:  map[SG]int{},
	}
}

func (h *Host) Join(g string) {
	h.joined[g] = true
	h.Seg.transmitMsg(nil, h.ID, Msg{Kind: MReport, From: h.ID, G: g})
}

func (h *Host) Leave(g string) {
	delete(h.joined, g)
	h.Seg.transmitMsg(nil, h.ID, Msg{Kind: MLeave, From: h.ID, G: g})
}

func (h *Host) onData(d DataPkt) {
	if !h.joined[d.G] {
		return
	}
	key := SG{d.S, d.G}
	seen := h.recv[key]
	if seen == nil {
		seen = map[int]bool{}
		h.recv[key] = seen
	}
	if seen[d.Seq] {
		h.dups[key]++
		return
	}
	seen[d.Seq] = true
	if _, ok := h.first[key]; !ok {
		h.first[key] = d.Seq
	}
}

// Stats 返回 (收到, 丢失, 重复)。丢失按 [第一个收到的序号, maxSeq] 内的空洞统计,
// 因此树建立之前的序号不计入,而运行中途(切换/断链)的丢包会被完整记录。
func (h *Host) Stats(key SG, maxSeq int) (recv, lost, dup int) {
	seen := h.recv[key]
	if len(seen) == 0 {
		return 0, 0, h.dups[key]
	}
	for s := h.first[key]; s <= maxSeq; s++ {
		if !seen[s] {
			lost++
		}
	}
	return len(seen), lost, h.dups[key]
}

// 组播源
type Source struct {
	ID       string
	Seg      *Seg
	Sim      *Sim
	G        string
	Active   bool
	Seq      int
	Interval int64
	LastSent int64
}

func (s *Source) Start() {
	if s.Active {
		return
	}
	s.Active = true
	s.tick()
}

func (s *Source) Stop() { s.Active = false }

func (s *Source) tick() {
	if !s.Active {
		return
	}
	s.Seq++
	s.LastSent = s.Sim.Now
	s.Seg.transmitData(nil, s.ID, DataPkt{S: s.ID, G: s.G, Seq: s.Seq})
	s.Sim.After(s.Interval, s.tick)
}
