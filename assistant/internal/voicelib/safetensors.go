package voicelib

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxStateBytes is the largest voice state the library accepts. A Pocket TTS
// voice state is a few megabytes; 32 MB leaves room for a large context while
// keeping a copied file bounded and cheap to header-check.
const MaxStateBytes int64 = 32 << 20

// Safetensors layout constants. The file is an 8-byte little-endian header
// length, a JSON header, then the tensor data. Reading the header never asks a
// tensor library to deserialize the file, so a pickle payload cannot run.
const (
	safetensorsHeaderBytes = 8
	maxSafetensorsHeader   = 1 << 20
	f32Size                = 4
	i64Size                = 8
	// Pocket TTS exports a streaming state for six FlowLM layers. Each layer
	// has one attention cache; offset and optional pad tensors describe where
	// the prompt ends. #64 reuses this check before copying a file in.
	requiredStateTensors = 12
	optionalStateTensors = 6
	stateLayerCount      = 6
	stateHeads           = 16
	stateDimPerHead      = 64
	maxStateContext      = 400
)

// StateInfo reports the structure of a validated voice state.
type StateInfo struct {
	Tensors    int
	Layers     int
	ContextLen int
	DataBytes  int64
}

// tensorInfo is one entry in the safetensors JSON header.
type tensorInfo struct {
	Dtype       string   `json:"dtype"`
	Shape       []int64  `json:"shape"`
	DataOffsets [2]int64 `json:"data_offsets"`
}

// InspectState validates the safetensors header of a copied voice state without
// loading any tensor data. It rejects the pickle formats (.pt/.pth/.bin) by
// extension, enforces the 32 MB ceiling, and checks the Pocket TTS streaming
// state shape the pinned model exports:
//
//   - exactly six `self_attn/cache` F32 tensors of shape [2, 1, N, 16, 64],
//     1 <= N <= 400;
//   - exactly six matching `self_attn/offset` I64 [1] tensors;
//   - optionally six `self_attn/pad` I64 [1] tensors (12 or 18 entries total);
//   - data offsets inside the file, non-overlapping, and covering all bytes.
//
// The same function is the upload gate for #64: write the upload to a temp file
// under the library directory, call InspectState, then rename it into place.
func InspectState(path string) (StateInfo, error) {
	if !strings.EqualFold(filepath.Ext(path), ".safetensors") {
		return StateInfo{}, fmt.Errorf("voice state: only .safetensors files are supported, got %q", filepath.Ext(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return StateInfo{}, fmt.Errorf("voice state: open: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return StateInfo{}, fmt.Errorf("voice state: stat: %w", err)
	}
	size := info.Size()
	if size > MaxStateBytes {
		return StateInfo{}, fmt.Errorf("voice state: %d bytes is over the %d MB limit", size, MaxStateBytes>>20)
	}
	if size < safetensorsHeaderBytes+2 {
		return StateInfo{}, fmt.Errorf("voice state: file is too small to be safetensors")
	}

	var length [safetensorsHeaderBytes]byte
	if _, err := io.ReadFull(file, length[:]); err != nil {
		return StateInfo{}, fmt.Errorf("voice state: read header length: %w", err)
	}
	headerLen := int64(binary.LittleEndian.Uint64(length[:]))
	if headerLen <= 0 || headerLen > maxSafetensorsHeader {
		return StateInfo{}, fmt.Errorf("voice state: header length %d is outside (0, %d]", headerLen, maxSafetensorsHeader)
	}
	if safetensorsHeaderBytes+headerLen > size {
		return StateInfo{}, fmt.Errorf("voice state: header length %d runs past the %d-byte file", headerLen, size)
	}

	header := make([]byte, headerLen)
	if _, err := io.ReadFull(file, header); err != nil {
		return StateInfo{}, fmt.Errorf("voice state: read header: %w", err)
	}
	// json.Unmarshal rejects the non-JSON header of a torch pickle before any
	// bytes reach a tensor loader.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(header, &raw); err != nil {
		return StateInfo{}, fmt.Errorf("voice state: header is not safetensors JSON: %w", err)
	}

	dataBytes := size - safetensorsHeaderBytes - headerLen
	tensors := make(map[string]tensorInfo, len(raw))
	for key, entry := range raw {
		if key == "__metadata__" {
			// Standard safetensors metadata is not a tensor and carries no data.
			continue
		}
		var tensor tensorInfo
		if err := json.Unmarshal(entry, &tensor); err != nil {
			return StateInfo{}, fmt.Errorf("voice state: tensor %q: %w", key, err)
		}
		tensors[key] = tensor
	}

	return validateStateTensors(tensors, dataBytes)
}

// validateStateTensors applies the structural policy to the decoded header.
func validateStateTensors(tensors map[string]tensorInfo, dataBytes int64) (StateInfo, error) {
	if len(tensors) != requiredStateTensors && len(tensors) != requiredStateTensors+optionalStateTensors {
		return StateInfo{}, fmt.Errorf("voice state: has %d tensors, want %d (or %d with optional pad tensors)",
			len(tensors), requiredStateTensors, requiredStateTensors+optionalStateTensors)
	}

	layers := make(map[string]bool, stateLayerCount)
	var contextLen int64
	caches, offsets, pads := 0, 0, 0
	for key, tensor := range tensors {
		module, name, ok := strings.Cut(key, "/")
		if !ok || module == "" || name == "" {
			return StateInfo{}, fmt.Errorf("voice state: tensor %q is not <module>/<tensor>", key)
		}
		base := baseModule(module)
		if base == "" {
			return StateInfo{}, fmt.Errorf("voice state: tensor %q has no self_attn module", key)
		}
		layers[base] = true
		switch name {
		case "cache":
			caches++
			if tensor.Dtype != "F32" {
				return StateInfo{}, fmt.Errorf("voice state: %s has dtype %q, want F32", key, tensor.Dtype)
			}
			if shape := tensor.Shape; len(shape) != 5 ||
				shape[0] != 2 || shape[1] != 1 ||
				shape[2] < 1 || shape[2] > maxStateContext ||
				shape[3] != stateHeads || shape[4] != stateDimPerHead {
				return StateInfo{}, fmt.Errorf("voice state: %s has shape %v, want [2 1 N %d %d] with 1 <= N <= %d",
					key, tensor.Shape, stateHeads, stateDimPerHead, maxStateContext)
			}
			if contextLen == 0 {
				contextLen = tensor.Shape[2]
			} else if tensor.Shape[2] != contextLen {
				return StateInfo{}, fmt.Errorf("voice state: %s has context %d, other layers use %d", key, tensor.Shape[2], contextLen)
			}
		case "offset":
			offsets++
			if err := validateSmallIntTensor(key, tensor); err != nil {
				return StateInfo{}, err
			}
		case "pad":
			pads++
			if err := validateSmallIntTensor(key, tensor); err != nil {
				return StateInfo{}, err
			}
		default:
			return StateInfo{}, fmt.Errorf("voice state: unexpected tensor %q; only self_attn cache, offset and pad are allowed", key)
		}
	}
	if caches != stateLayerCount || offsets != stateLayerCount {
		return StateInfo{}, fmt.Errorf("voice state: has %d caches and %d offsets, want %d of each", caches, offsets, stateLayerCount)
	}
	if pads != 0 && pads != stateLayerCount {
		return StateInfo{}, fmt.Errorf("voice state: has %d pad tensors, want 0 or %d", pads, stateLayerCount)
	}
	if len(layers) != stateLayerCount {
		return StateInfo{}, fmt.Errorf("voice state: has %d self_attn modules, want %d", len(layers), stateLayerCount)
	}

	// Every data byte must belong to exactly one tensor and stay inside the
	// file. This is what makes the header check safe for a file another process
	// will later hand to safetensors/torch.
	ranges := make([]struct {
		key        string
		begin, end int64
	}, 0, len(tensors))
	expected := int64(0)
	for key, tensor := range tensors {
		begin, end := tensor.DataOffsets[0], tensor.DataOffsets[1]
		if begin < 0 || end <= begin || end > dataBytes {
			return StateInfo{}, fmt.Errorf("voice state: %s offsets [%d %d] are outside the %d data bytes", key, begin, end, dataBytes)
		}
		if err := checkTensorBytes(key, tensor); err != nil {
			return StateInfo{}, err
		}
		ranges = append(ranges, struct {
			key        string
			begin, end int64
		}{key, begin, end})
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].begin < ranges[j].begin })
	for _, span := range ranges {
		if span.begin != expected {
			return StateInfo{}, fmt.Errorf("voice state: tensor %q starts at %d, want %d (data must be contiguous)", span.key, span.begin, expected)
		}
		expected = span.end
	}
	if expected != dataBytes {
		return StateInfo{}, fmt.Errorf("voice state: data is %d bytes, header describes %d", dataBytes, expected)
	}
	return StateInfo{Tensors: len(tensors), Layers: len(layers), ContextLen: int(contextLen), DataBytes: dataBytes}, nil
}

// baseModule returns the "<layer>.self_attn" prefix of a state tensor, or ""
// when the key is not an attention-state entry.
func baseModule(module string) string {
	if module == "self_attn" {
		return module
	}
	if strings.HasSuffix(module, "self_attn") || strings.HasSuffix(module, ".self_attn") {
		return module
	}
	if i := strings.LastIndex(module, "self_attn"); i >= 0 {
		return module[:i+len("self_attn")]
	}
	return ""
}

// validateSmallIntTensor checks an offset or pad entry: I64 with shape [1].
func validateSmallIntTensor(key string, tensor tensorInfo) error {
	if tensor.Dtype != "I64" {
		return fmt.Errorf("voice state: %s has dtype %q, want I64", key, tensor.Dtype)
	}
	if len(tensor.Shape) != 1 || tensor.Shape[0] != 1 {
		return fmt.Errorf("voice state: %s has shape %v, want [1]", key, tensor.Shape)
	}
	return nil
}

// checkTensorBytes proves the declared shape and dtype match the byte range, so
// a malformed header cannot make a loader read outside its allocation.
func checkTensorBytes(key string, tensor tensorInfo) error {
	elements := int64(1)
	for _, dim := range tensor.Shape {
		if dim < 0 {
			return fmt.Errorf("voice state: %s has negative dimension %d", key, dim)
		}
		if dim != 0 && elements > (1<<40)/dim {
			return fmt.Errorf("voice state: %s shape overflows", key)
		}
		elements *= dim
	}
	width := int64(0)
	switch tensor.Dtype {
	case "F32":
		width = f32Size
	case "I64":
		width = i64Size
	default:
		return fmt.Errorf("voice state: %s has unsupported dtype %q", key, tensor.Dtype)
	}
	want := elements * width
	got := tensor.DataOffsets[1] - tensor.DataOffsets[0]
	if got != want {
		return fmt.Errorf("voice state: %s shape %v needs %d data bytes, header gives %d", key, tensor.Shape, want, got)
	}
	return nil
}
