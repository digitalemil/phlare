//go:build linux

// Package ebpfspy provides integration with Linux eBPF. It is a rough copy of profile.py from BCC tools:
//
//	https://github.com/iovisor/bcc/blob/master/tools/profile.py
package ebpfspy

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/phlare/ebpf/cpuonline"
	"github.com/grafana/phlare/ebpf/sd"
	"github.com/grafana/phlare/ebpf/symtab"
	"github.com/samber/lo"
)

//go:generate make -C bpf get-headers
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -cflags "-O2 -Wall -fpie -Wno-unused-variable -Wno-unused-function" profile bpf/profile.bpf.c -- -I./bpf/libbpf -I./bpf/vmlinux/

type SessionOptions struct {
	CollectUser   bool
	CollectKernel bool
	CacheOptions  symtab.CacheOptions
	SampleRate    int
}

type Session interface {
	Start() error
	Stop()
	Update(SessionOptions) error
	CollectProfiles(f func(target *sd.Target, stack []string, value uint64, pid uint32)) error
	DebugInfo() interface{}
}

type SessionDebugInfo struct {
	ElfCache symtab.ElfCacheDebugInfo                          `river:"elf_cache,attr,optional"`
	PidCache symtab.GCacheDebugInfo[symtab.ProcTableDebugInfo] `river:"pid_cache,attr,optional"`
}

type session struct {
	logger log.Logger
	pid    int

	targetFinder sd.TargetFinder

	perfEvents []*perfEvent

	symCache *symtab.SymbolCache

	bpf profileObjects

	options     SessionOptions
	roundNumber int
}

func NewSession(
	logger log.Logger,
	targetFinder sd.TargetFinder,

	sessionOptions SessionOptions,
) (Session, error) {

	symCache, err := symtab.NewSymbolCache(logger, sessionOptions.CacheOptions)
	if err != nil {
		return nil, err
	}

	return &session{
		logger:   logger,
		pid:      -1,
		symCache: symCache,

		targetFinder: targetFinder,
		options:      sessionOptions,
	}, nil
}

func (s *session) Start() error {
	var err error

	if err = rlimit.RemoveMemlock(); err != nil {
		return err
	}

	opts := &ebpf.CollectionOptions{}
	if err := loadProfileObjects(&s.bpf, opts); err != nil {
		return fmt.Errorf("load bpf objects: %w", err)
	}
	if err = s.initArgs(); err != nil {
		return fmt.Errorf("init bpf args: %w", err)
	}
	if err = s.attachPerfEvents(); err != nil {
		return fmt.Errorf("attach perf events: %w", err)
	}
	return nil
}

func (s *session) CollectProfiles(cb func(t *sd.Target, stack []string, value uint64, pid uint32)) error {
	defer s.symCache.Cleanup()

	s.symCache.NextRound()
	s.roundNumber++

	keys, values, batch, err := s.getCountsMapValues()
	if err != nil {
		return fmt.Errorf("get counts map: %w", err)
	}

	type sf struct {
		pid    uint32
		count  uint32
		kStack []byte
		uStack []byte
		comm   string
		labels *sd.Target
	}
	var sfs []sf
	knownStacks := map[uint32]bool{}
	for i := range keys {
		ck := &keys[i]
		value := values[i]

		if ck.UserStack >= 0 {
			knownStacks[uint32(ck.UserStack)] = true
		}
		if ck.KernStack >= 0 {
			knownStacks[uint32(ck.KernStack)] = true
		}
		labels := s.targetFinder.FindTarget(ck.Pid)
		if labels == nil {
			continue
		}
		uStack := s.getStack(ck.UserStack)
		kStack := s.getStack(ck.KernStack)
		sfs = append(sfs, sf{
			pid:    ck.Pid,
			uStack: uStack,
			kStack: kStack,
			count:  value,
			comm:   getComm(ck),
			labels: labels,
		})
	}

	sb := stackBuilder{}
	for _, it := range sfs {
		sb.rest()
		sb.append(it.comm)
		if s.options.CollectUser {
			s.walkStack(&sb, it.uStack, it.pid)
		}
		if s.options.CollectKernel {
			s.walkStack(&sb, it.kStack, 0)
		}
		lo.Reverse(sb.stack)
		cb(it.labels, sb.stack, uint64(it.count), it.pid)
	}
	if err = s.clearCountsMap(keys, batch); err != nil {
		return fmt.Errorf("clear counts map %w", err)
	}
	if err = s.clearStacksMap(knownStacks); err != nil {
		return fmt.Errorf("clear stacks map %w", err)
	}
	return nil
}

func (s *session) Stop() {
	for _, pe := range s.perfEvents {
		_ = pe.Close()
	}
	s.perfEvents = nil
	s.bpf.Close()
}

func (s *session) Update(options SessionOptions) error {
	s.symCache.UpdateOptions(options.CacheOptions)
	err := s.updateSampleRate(options.SampleRate)
	if err != nil {
		return err
	}
	s.options = options
	return nil
}

func (s *session) DebugInfo() interface{} {
	return SessionDebugInfo{
		ElfCache: s.symCache.ElfCacheDebugInfo(),
		PidCache: s.symCache.PidCacheDebugInfo(),
	}
}

func (s *session) initArgs() error {
	var zero uint32
	var tgidFilter uint32
	if s.pid <= 0 {
		tgidFilter = 0
	} else {
		tgidFilter = uint32(s.pid)
	}
	arg := &profileBssArg{
		TgidFilter: tgidFilter,
	}
	if err := s.bpf.Args.Update(&zero, arg, 0); err != nil {
		return fmt.Errorf("init args fail: %w", err)
	}
	return nil
}

func (s *session) attachPerfEvents() error {
	var cpus []uint
	var err error
	if cpus, err = cpuonline.Get(); err != nil {
		return fmt.Errorf("get cpuonline: %w", err)
	}
	for _, cpu := range cpus {
		pe, err := newPerfEvent(int(cpu), s.options.SampleRate)
		if err != nil {
			return fmt.Errorf("new perf event: %w", err)
		}
		s.perfEvents = append(s.perfEvents, pe)

		err = pe.attachPerfEvent(s.bpf.profilePrograms.DoPerfEvent)
		if err != nil {
			return fmt.Errorf("attach perf event: %w", err)
		}
	}
	return nil
}

func (s *session) getStack(stackId int64) []byte {
	if stackId < 0 {
		return nil
	}
	stackIdU32 := uint32(stackId)
	res, err := s.bpf.profileMaps.Stacks.LookupBytes(stackIdU32)
	if err != nil {
		return nil
	}
	return res
}

// stack is an array of 127 uint64s, where each uint64 is an instruction pointer
func (s *session) walkStack(sb *stackBuilder, stack []byte, pid uint32) {
	if len(stack) == 0 {
		return
	}
	var stackFrames []string
	for i := 0; i < 127; i++ {
		instructionPointerBytes := stack[i*8 : i*8+8]
		instructionPointer := binary.LittleEndian.Uint64(instructionPointerBytes)
		if instructionPointer == 0 {
			break
		}
		sym := s.symCache.Resolve(pid, instructionPointer)
		var name string
		if sym.Name != "" {
			name = sym.Name
		} else {
			if sym.Module != "" {
				//name = fmt.Sprintf("%s+%x", sym.Module, sym.Start) // todo expose an option to enable this
				name = sym.Module
			} else {
				name = "[unknown]"
			}
		}
		stackFrames = append(stackFrames, name)
	}
	lo.Reverse(stackFrames)
	for _, s := range stackFrames {
		sb.append(s)
	}
}

func (s *session) updateSampleRate(sampleRate int) error {
	if s.options.SampleRate == sampleRate {
		return nil
	}
	_ = level.Debug(s.logger).Log(
		"sample_rate_new", sampleRate,
		"sample_rate_old", s.options.SampleRate,
	)
	s.Stop()
	s.options.SampleRate = sampleRate
	err := s.Start()
	if err != nil {
		return fmt.Errorf("ebpf restart: %w", err)
	}

	return nil
}

func getComm(k *profileSampleKey) string {
	res := ""
	// todo remove unsafe

	sh := (*reflect.StringHeader)(unsafe.Pointer(&res))
	sh.Data = uintptr(unsafe.Pointer(&k.Comm[0]))
	for _, c := range k.Comm {
		if c != 0 {
			sh.Len++
		} else {
			break
		}
	}
	return res
}

type stackBuilder struct {
	stack []string
}

func (s *stackBuilder) rest() {
	s.stack = s.stack[:0]
}

func (s *stackBuilder) append(sym string) {
	s.stack = append(s.stack, sym)
}