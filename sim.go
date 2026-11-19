package main

import "container/heap"

// 离散事件仿真时钟,所有协议定时器、链路时延都排进同一个事件堆,
// 测试时按虚拟时间推进,保证结果确定、可复现。

type Event struct {
	at  int64
	seq int
	fn  func()
}

type eventHeap []Event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].at != h[j].at {
		return h[i].at < h[j].at
	}
	return h[i].seq < h[j].seq
}
func (h eventHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x interface{}) { *h = append(*h, x.(Event)) }
func (h *eventHeap) Pop() interface{} {
	old := *h
	n := len(old)
	e := old[n-1]
	*h = old[:n-1]
	return e
}

type Sim struct {
	Now int64 // 虚拟时间,毫秒
	pq  eventHeap
	seq int
}

func (s *Sim) After(d int64, fn func()) {
	heap.Push(&s.pq, Event{at: s.Now + d, seq: s.seq, fn: fn})
	s.seq++
}

func (s *Sim) Step() bool {
	if len(s.pq) == 0 {
		return false
	}
	e := heap.Pop(&s.pq).(Event)
	s.Now = e.at
	e.fn()
	return true
}

// RunUntil 把仿真推进到时刻 t(处理所有 at<=t 的事件)
func (s *Sim) RunUntil(t int64) {
	for len(s.pq) > 0 && s.pq[0].at <= t {
		s.Step()
	}
	s.Now = t
}
