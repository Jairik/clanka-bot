// Package voicelibtest builds synthetic safetensors voice states for tests.
// The real pinned export has six FlowLM attention layers; these fixtures keep
// the same header shape with a tiny context so tests stay fast.
package voicelibtest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Tensor is one header entry and its raw little-endian data.
type Tensor struct {
	Dtype string
	Shape []int64
	Data  []byte
}

// Write serialises tensors as a safetensors file, computing contiguous data
// offsets in sorted key order.
func Write(path string, tensors map[string]Tensor) error {
	keys := make([]string, 0, len(tensors))
	for key := range tensors {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	type entry struct {
		Dtype       string   `json:"dtype"`
		Shape       []int64  `json:"shape"`
		DataOffsets [2]int64 `json:"data_offsets"`
	}
	header := make(map[string]entry, len(keys))
	offset := int64(0)
	var data []byte
	for _, key := range keys {
		tensor := tensors[key]
		end := offset + int64(len(tensor.Data))
		header[key] = entry{Dtype: tensor.Dtype, Shape: tensor.Shape, DataOffsets: [2]int64{offset, end}}
		data = append(data, tensor.Data...)
		offset = end
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(raw)))
	if _, err := file.Write(length[:]); err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return nil
}

// WriteValid builds a structurally valid Pocket TTS state. contextLen controls
// the cache shape [2,1,N,16,64]; withPads selects the 18-tensor real export
// layout instead of the 12-tensor cache+offset form.
func WriteValid(path string, contextLen int, withPads bool) error {
	if contextLen <= 0 {
		return fmt.Errorf("contextLen must be positive")
	}
	tensors := make(map[string]Tensor, 18)
	cacheBytes := 2 * 1 * contextLen * 16 * 64 * 4
	for layer := 0; layer < 6; layer++ {
		prefix := fmt.Sprintf("transformer.layers.%d.self_attn", layer)
		tensors[prefix+"/offset"] = Tensor{Dtype: "I64", Shape: []int64{1}, Data: make([]byte, 8)}
		if withPads {
			tensors[prefix+"/pad"] = Tensor{Dtype: "I64", Shape: []int64{1}, Data: make([]byte, 8)}
		}
		tensors[prefix+"/cache"] = Tensor{
			Dtype: "F32",
			Shape: []int64{2, 1, int64(contextLen), 16, 64},
			Data:  make([]byte, cacheBytes),
		}
	}
	return Write(path, tensors)
}
