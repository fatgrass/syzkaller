// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"fmt"
	"math/rand"
	"sort"

	"github.com/google/syzkaller/sys"
)

// Calulation of call-to-call priorities.
// For a given pair of calls X and Y, the priority is our guess as to whether
// additional of call Y into a program containing call X is likely to give
// new coverage or not.
// The current algorithm has two components: static and dynamic.
// The static component is based on analysis of argument types. For example,
// if call X and call Y both accept fd[sock], then they are more likely to give
// new coverage together.
// The dynamic component is based on frequency of occurrence of a particular
// pair of syscalls in a single program in corpus. For example, if socket and
// connect frequently occur in programs together, we give higher priority to
// this pair of syscalls.
// Note: the current implementation is very basic, there is no theory behind any
// constants.

func CalculatePriorities(corpus []*Prog) [][]float32 {
	static := calcStaticPriorities()
	dynamic := calcDynamicPrio(corpus)
	for i, prios := range static {
		for j, p := range prios {
			dynamic[i][j] *= p
		}
	}
	return dynamic
}

func calcStaticPriorities() [][]float32 {
	uses := make(map[string]map[int]float32)
	for _, c := range sys.Calls {
		noteUsage := func(weight float32, str string, args ...interface{}) {
			id := fmt.Sprintf(str, args...)
			if uses[id] == nil {
				uses[id] = make(map[int]float32)
			}
			old := uses[id][c.ID]
			if weight > old {
				uses[id][c.ID] = weight
			}
		}
		foreachArgType(c, func(t sys.Type, d ArgDir) {
			switch a := t.(type) {
			case sys.ResourceType:
				if a.Kind == sys.ResPid || a.Kind == sys.ResUid || a.Kind == sys.ResGid {
					// Pid/uid/gid usually play auxiliary role,
					// but massively happen in some structs.
					noteUsage(0.1, "res%v", a.Kind)
				} else if a.Subkind == sys.ResAny {
					noteUsage(1.0, "res%v", a.Kind)
				} else {
					noteUsage(0.2, "res%v", a.Kind)
					noteUsage(1.0, "res%v-%v", a.Kind, a.Subkind)
				}
			case sys.PtrType:
				if _, ok := a.Type.(sys.StructType); ok {
					noteUsage(1.0, "ptrto-%v", a.Type.Name())
				}
				if _, ok := a.Type.(sys.UnionType); ok {
					noteUsage(1.0, "ptrto-%v", a.Type.Name())
				}
				if arr, ok := a.Type.(sys.ArrayType); ok {
					noteUsage(1.0, "ptrto-%v", arr.Type.Name())
				}
			case sys.BufferType:
				switch a.Kind {
				case sys.BufferBlob, sys.BufferFilesystem, sys.BufferAlgType, sys.BufferAlgName:
				case sys.BufferString:
					noteUsage(0.2, "str")
				case sys.BufferSockaddr:
					noteUsage(1.0, "sockaddr")
				default:
					panic("unknown buffer kind")
				}
			case sys.VmaType:
				noteUsage(0.5, "vma")
			case sys.FilenameType:
				noteUsage(1.0, "filename")
			case sys.IntType:
				switch a.Kind {
				case sys.IntPlain:
				case sys.IntSignalno:
					noteUsage(1.0, "signalno")
				case sys.IntInaddr:
					noteUsage(1.0, "inaddr")
				case sys.IntInport:
					noteUsage(1.0, "inport")
				default:
					panic("unknown int kind")
				}
			}
		})
	}
	prios := make([][]float32, len(sys.Calls))
	for i := range prios {
		prios[i] = make([]float32, len(sys.Calls))
	}
	for _, calls := range uses {
		for c0, w0 := range calls {
			for c1, w1 := range calls {
				if c0 == c1 {
					// Self-priority is assigned below.
					continue
				}
				prios[c0][c1] += w0 * w1
			}
		}
	}

	// Sockets essentially form another level of classification. And we want
	// more priority for generic socket calls (e.g. recvmsg) against particular
	// protocol socket calls (e.g. AF_ALG bind). But it is not expressable with
	// the above uses thing, because we don't want more priority for different
	// protocols (e.g. AF_ALF vs AF_BLUETOOTH).
	for c0, w0 := range uses[fmt.Sprintf("res%v-%v", sys.ResFD, sys.FdSock)] {
		for _, sk := range sys.SocketSubkinds() {
			for c1, w1 := range uses[fmt.Sprintf("res%v-%v", sys.ResFD, sk)] {
				prios[c0][c1] += w0 * w1
				prios[c1][c0] += w0 * w1
			}
		}
	}

	// Self-priority (call wrt itself) is assigned to the maximum priority
	// this call has wrt other calls. This way the priority is high, but not too high.
	for c0, pp := range prios {
		var max float32
		for _, p := range pp {
			if max < p {
				max = p
			}
		}
		pp[c0] = max
	}
	normalizePrio(prios)
	return prios
}

func calcDynamicPrio(corpus []*Prog) [][]float32 {
	prios := make([][]float32, len(sys.Calls))
	for i := range prios {
		prios[i] = make([]float32, len(sys.Calls))
	}
	for _, p := range corpus {
		for i0 := 0; i0 < len(p.Calls); i0++ {
			for i1 := 0; i1 < len(p.Calls); i1++ {
				if i0 == i1 {
					continue
				}
				prios[i0][i1] += 1.0
			}
		}
	}
	normalizePrio(prios)
	return prios
}

// normalizePrio assigns some minimal priorities to calls with zero priority,
// and then normalizes priorities to 0.1..1 range.
func normalizePrio(prios [][]float32) {
	for _, prio := range prios {
		max := float32(0)
		min := float32(1e10)
		nzero := 0
		for _, p := range prio {
			if max < p {
				max = p
			}
			if p != 0 && min > p {
				min = p
			}
			if p == 0 {
				nzero++
			}
		}
		if nzero != 0 {
			min /= 2 * float32(nzero)
		}
		for i, p := range prio {
			if max == 0 {
				prio[i] = 1
				continue
			}
			if p == 0 {
				p = min
			}
			p = (p-min)/(max-min)*0.9 + 0.1
			if p > 1 {
				p = 1
			}
			prio[i] = p
		}
	}
}

func foreachArgType(meta *sys.Call, f func(sys.Type, ArgDir)) {
	var rec func(t sys.Type, dir ArgDir)
	rec = func(t sys.Type, d ArgDir) {
		f(t, d)
		switch a := t.(type) {
		case sys.ArrayType:
			rec(a.Type, d)
		case sys.PtrType:
			rec(a.Type, ArgDir(a.Dir))
		case sys.StructType:
			for _, f := range a.Fields {
				rec(f, d)
			}
		case sys.UnionType:
			for _, opt := range a.Options {
				rec(opt, d)
			}
		case sys.ResourceType, sys.FileoffType, sys.BufferType,
			sys.VmaType, sys.LenType, sys.FlagsType, sys.ConstType,
			sys.StrConstType, sys.IntType, sys.FilenameType:
		default:
			panic("unknown type")
		}
	}
	for _, t := range meta.Args {
		rec(t, DirIn)
	}
	if meta.Ret != nil {
		rec(meta.Ret, DirOut)
	}
}

// ChooseTable allows to do a weighted choice of a syscall for a given syscall
// based on call-to-call priorities and a set of enabled syscalls.
type ChoiceTable struct {
	run          [][]int
	enabledCalls []*sys.Call
	enabled      map[*sys.Call]bool
}

func BuildChoiceTable(prios [][]float32, enabled map[*sys.Call]bool) *ChoiceTable {
	if enabled == nil {
		enabled = make(map[*sys.Call]bool)
		for _, c := range sys.Calls {
			enabled[c] = true
		}
	}
	var enabledCalls []*sys.Call
	for c := range enabled {
		enabledCalls = append(enabledCalls, c)
	}
	run := make([][]int, len(sys.Calls))
	for i := range run {
		if !enabled[sys.Calls[i]] {
			continue
		}
		run[i] = make([]int, len(sys.Calls))
		sum := 0
		for j := range run[i] {
			if enabled[sys.Calls[j]] {
				sum += int(prios[i][j] * 1000)
			}
			run[i][j] = sum
		}
	}
	return &ChoiceTable{run, enabledCalls, enabled}
}

func (ct *ChoiceTable) Choose(r *rand.Rand, call int) int {
	if ct == nil {
		return r.Intn(len(sys.Calls))
	}
	if call < 0 {
		return ct.enabledCalls[r.Intn(len(ct.enabledCalls))].ID
	}
	run := ct.run[call]
	if run == nil {
		return ct.enabledCalls[r.Intn(len(ct.enabledCalls))].ID
	}
	for {
		x := r.Intn(run[len(run)-1])
		i := sort.SearchInts(run, x)
		if !ct.enabled[sys.Calls[i]] {
			continue
		}
		return i
	}
}
