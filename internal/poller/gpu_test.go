package poller

import (
	"testing"

	"github.com/awlx/vllm-dashboard/internal/collect/gpu"
	"github.com/awlx/vllm-dashboard/internal/collect/process"
	"github.com/awlx/vllm-dashboard/internal/sample"
)

func TestAttributeGPUModels(t *testing.T) {
	reading := gpu.Reading{
		GPUs:  []sample.GPUStat{{Index: 0, UUID: "GPU-a"}, {Index: 1, UUID: "GPU-b"}},
		Procs: []gpu.ProcMem{{PID: 1, UUID: "GPU-a", MB: 100}, {PID: 2, UUID: "GPU-a", MB: 50}, {PID: 3, UUID: "GPU-b", MB: 75}, {PID: 4, UUID: "GPU-unknown", MB: 10}},
	}
	keys := map[int]string{1: "model-a", 2: "model-a", 3: process.OtherModel, 4: process.OtherModel}
	got := attributeGPUModels(reading, keys)
	want := []sample.GPUModelMem{
		{GPU: 0, Model: "model-a", MB: 150},
		{GPU: 1, Model: process.OtherModel, MB: 75},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i, mem := range want {
		if got[i] != mem {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], mem)
		}
	}
	if attributeGPUModels(gpu.Reading{}, nil) != nil {
		t.Fatal("empty reading should have no model memory")
	}
}
