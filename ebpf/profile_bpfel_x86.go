// Code generated by bpf2go; DO NOT EDIT.
//go:build 386 || amd64

package ebpfspy

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"

	"github.com/cilium/ebpf"
)

type profileBssArg struct {
	TgidFilter    uint32
	CollectUser   uint8
	CollectKernel uint8
	_             [2]byte
}

type profileOutStack [127]uint64

type profileSampleKey struct {
	Pid       uint32
	Flags     uint32
	KernStack int64
	UserStack int64
	Comm      [16]int8
}

// loadProfile returns the embedded CollectionSpec for profile.
func loadProfile() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_ProfileBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load profile: %w", err)
	}

	return spec, err
}

// loadProfileObjects loads profile and converts it into a struct.
//
// The following types are suitable as obj argument:
//
//	*profileObjects
//	*profilePrograms
//	*profileMaps
//
// See ebpf.CollectionSpec.LoadAndAssign documentation for details.
func loadProfileObjects(obj interface{}, opts *ebpf.CollectionOptions) error {
	spec, err := loadProfile()
	if err != nil {
		return err
	}

	return spec.LoadAndAssign(obj, opts)
}

// profileSpecs contains maps and programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type profileSpecs struct {
	profileProgramSpecs
	profileMapSpecs
}

// profileSpecs contains programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type profileProgramSpecs struct {
	DoPerfEvent *ebpf.ProgramSpec `ebpf:"do_perf_event"`
}

// profileMapSpecs contains maps before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type profileMapSpecs struct {
	Args          *ebpf.MapSpec `ebpf:"args"`
	Counts        *ebpf.MapSpec `ebpf:"counts"`
	ManualStacks  *ebpf.MapSpec `ebpf:"manual_stacks"`
	ScratchStacks *ebpf.MapSpec `ebpf:"scratch_stacks"`
	Stacks        *ebpf.MapSpec `ebpf:"stacks"`
}

// profileObjects contains all objects after they have been loaded into the kernel.
//
// It can be passed to loadProfileObjects or ebpf.CollectionSpec.LoadAndAssign.
type profileObjects struct {
	profilePrograms
	profileMaps
}

func (o *profileObjects) Close() error {
	return _ProfileClose(
		&o.profilePrograms,
		&o.profileMaps,
	)
}

// profileMaps contains all maps after they have been loaded into the kernel.
//
// It can be passed to loadProfileObjects or ebpf.CollectionSpec.LoadAndAssign.
type profileMaps struct {
	Args          *ebpf.Map `ebpf:"args"`
	Counts        *ebpf.Map `ebpf:"counts"`
	ManualStacks  *ebpf.Map `ebpf:"manual_stacks"`
	ScratchStacks *ebpf.Map `ebpf:"scratch_stacks"`
	Stacks        *ebpf.Map `ebpf:"stacks"`
}

func (m *profileMaps) Close() error {
	return _ProfileClose(
		m.Args,
		m.Counts,
		m.ManualStacks,
		m.ScratchStacks,
		m.Stacks,
	)
}

// profilePrograms contains all programs after they have been loaded into the kernel.
//
// It can be passed to loadProfileObjects or ebpf.CollectionSpec.LoadAndAssign.
type profilePrograms struct {
	DoPerfEvent *ebpf.Program `ebpf:"do_perf_event"`
}

func (p *profilePrograms) Close() error {
	return _ProfileClose(
		p.DoPerfEvent,
	)
}

func _ProfileClose(closers ...io.Closer) error {
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Do not access this directly.
//
//go:embed profile_bpfel_x86.o
var _ProfileBytes []byte