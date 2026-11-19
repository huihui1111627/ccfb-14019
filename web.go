package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// 实时仿真 + HTTP API + 单页拓扑图。
// 页面轮询 /api/state?g=组,画当前树的形状和每台路由器的状态项。

//go:embed web/index.html
var indexHTML []byte

type WebSim struct {
	mu  sync.Mutex
	sim *Sim
	t   *Topo
}

func ServeWeb(addr string) {
	ws := &WebSim{}
	ws.sim = &Sim{}
	ws.t = BuildTopo(ws.sim)
	for _, r := range ws.t.Routers {
		r.Start()
	}
	// 5 倍速推进仿真
	go func() {
		for {
			time.Sleep(100 * time.Millisecond)
			ws.mu.Lock()
			ws.sim.RunUntil(ws.sim.Now + 500)
			ws.mu.Unlock()
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/topo", ws.handleTopo)
	http.HandleFunc("/api/state", ws.handleState)
	http.HandleFunc("/api/action", ws.handleAction)
	if err := http.ListenAndServe(addr, nil); err != nil {
		panic(err)
	}
}

func (ws *WebSim) handleTopo(w http.ResponseWriter, r *http.Request) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	type Node struct {
		ID   string `json:"id"`
		Kind string `json:"kind"` // router / host / source
		X    int    `json:"x"`
		Y    int    `json:"y"`
		RP   bool   `json:"rp,omitempty"`
	}
	type SegInfo struct {
		Name  string   `json:"name"`
		Cost  int      `json:"cost"`
		Nodes []string `json:"nodes"`
		X     int      `json:"x"`
		Y     int      `json:"y"`
	}
	out := struct {
		Nodes []Node    `json:"nodes"`
		Segs  []SegInfo `json:"segs"`
	}{}
	for id, p := range ws.t.Pos {
		kind := "host"
		if ws.t.Routers[id] != nil {
			kind = "router"
		}
		if ws.t.Sources[id] != nil {
			kind = "source"
		}
		out.Nodes = append(out.Nodes, Node{ID: id, Kind: kind, X: p[0], Y: p[1], RP: id == ws.t.RP})
	}
	for _, s := range ws.t.Segs {
		si := SegInfo{Name: s.Name, Cost: s.Cost, X: ws.t.SegPos[s.Name][0], Y: ws.t.SegPos[s.Name][1]}
		for _, if_ := range s.Ifs {
			si.Nodes = append(si.Nodes, if_.R.ID)
		}
		for _, h := range s.Hosts {
			si.Nodes = append(si.Nodes, h.ID)
		}
		for _, src := range s.Sources {
			si.Nodes = append(si.Nodes, src.ID)
		}
		out.Segs = append(out.Segs, si)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Segs, func(i, j int) bool { return out.Segs[i].Name < out.Segs[j].Name })
	json.NewEncoder(w).Encode(out)
}

func (ws *WebSim) handleState(w http.ResponseWriter, r *http.Request) {
	g := r.URL.Query().Get("g")
	if g == "" {
		g = "G1"
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := ws.sim.Now

	type RState struct {
		Key   string   `json:"key"`
		Iif   string   `json:"iif"`
		Oifs  []string `json:"oifs"`
		Flags []string `json:"flags"`
	}
	type RouterInfo struct {
		ID     string   `json:"id"`
		DR     []string `json:"dr"`
		States []RState `json:"states"`
	}
	type SegInfo struct {
		Name string         `json:"name"`
		Up   bool           `json:"up"`
		Tx   map[string]int `json:"tx"` // 流 -> 累计转发次数
	}
	type Edge struct {
		From string `json:"from"`
		To   string `json:"to"`
		Kind string `json:"kind"` // rpt / spt
	}
	type HostInfo struct {
		ID     string `json:"id"`
		Joined bool   `json:"joined"`
		Recv   int    `json:"recv"`
		Lost   int    `json:"lost"`
		Dup    int    `json:"dup"`
	}
	out := struct {
		Now     int64        `json:"now"`
		Group   string       `json:"group"`
		Routers []RouterInfo `json:"routers"`
		Segs    []SegInfo    `json:"segs"`
		Edges   []Edge       `json:"edges"`
		Hosts   []HostInfo   `json:"hosts"`
	}{Now: now, Group: g}

	ids := make([]string, 0, len(ws.t.Routers))
	for id := range ws.t.Routers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := ws.t.Routers[id]
		ri := RouterInfo{ID: id}
		for _, if_ := range r.Ifs {
			if r.IsDR(if_) && len(r.nbrs[if_]) > 0 {
				ri.DR = append(ri.DR, if_.Seg.Name)
			}
		}
		if st := r.G[g]; st != nil {
			rs := RState{Key: "(*," + g + ")", Iif: ifName(st.Iif)}
			for ifn := range actualRPTOifs(r, g) {
				rs.Oifs = append(rs.Oifs, ifn)
			}
			rs.Flags = append(rs.Flags, "上游→"+st.UpNbr)
			ri.States = append(ri.States, rs)
		}
		for key, st := range r.SGm {
			if key.G != g {
				continue
			}
			rs := RState{Key: "(" + key.S + "," + key.G + ")", Iif: ifName(st.Iif)}
			for if_ := range r.sgOifs(st) {
				rs.Oifs = append(rs.Oifs, if_.Name())
			}
			if st.SPTbit {
				rs.Flags = append(rs.Flags, "SPT")
			}
			rs.Flags = append(rs.Flags, "上游→"+st.UpNbr)
			ri.States = append(ri.States, rs)
			if st.UpNbr != "" && st.UpNbr != key.S {
				out.Edges = append(out.Edges, Edge{From: id, To: st.UpNbr, Kind: "spt"})
			}
		}
		if st := r.G[g]; st != nil && st.UpNbr != "" {
			out.Edges = append(out.Edges, Edge{From: id, To: st.UpNbr, Kind: "rpt"})
		}
		for key := range r.assertLost {
			if key.G == g {
				ri.States = append(ri.States, RState{Key: "assert-lost", Flags: []string{key.S + "@" + key.G}})
			}
		}
		sort.Slice(ri.States, func(i, j int) bool { return ri.States[i].Key < ri.States[j].Key })
		out.Routers = append(out.Routers, ri)
	}
	for _, s := range ws.t.Segs {
		si := SegInfo{Name: s.Name, Up: s.Up, Tx: map[string]int{}}
		for key, n := range s.TxCount {
			if key.G == g {
				si.Tx[key.S] = n
			}
		}
		out.Segs = append(out.Segs, si)
	}
	hids := make([]string, 0, len(ws.t.Hosts))
	for id := range ws.t.Hosts {
		hids = append(hids, id)
	}
	sort.Strings(hids)
	for _, id := range hids {
		h := ws.t.Hosts[id]
		hi := HostInfo{ID: id, Joined: h.joined[g]}
		for key := range h.recv {
			if key.G != g {
				continue
			}
			recv, lost, dup := h.Stats(key, ws.t.Sources[key.S].Seq)
			hi.Recv += recv
			hi.Lost += lost
			hi.Dup += dup
		}
		out.Hosts = append(out.Hosts, hi)
	}
	json.NewEncoder(w).Encode(out)
}

func (ws *WebSim) handleAction(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Op    string `json:"op"`    // join / leave / source / link
		Host  string `json:"host"`  // join/leave
		Group string `json:"group"` // join/leave
		Src   string `json:"src"`   // source
		On    bool   `json:"on"`    // source / link
		Seg   string `json:"seg"`   // link
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	switch a.Op {
	case "join":
		if h := ws.t.Hosts[a.Host]; h != nil {
			h.Join(a.Group)
		}
	case "leave":
		if h := ws.t.Hosts[a.Host]; h != nil {
			h.Leave(a.Group)
		}
	case "source":
		if s := ws.t.Sources[a.Src]; s != nil {
			if a.On {
				s.Start()
			} else {
				s.Stop()
			}
		}
	case "link":
		ws.t.SetSegUp(a.Seg, a.On)
	}
	w.Write([]byte("{}"))
}
