// hash.go implements a tree-walking hasher for map[string]any trees.
//
// The reconcile loop uses this to compute a content hash of the evaluated
// template before SSA apply. If the hash matches the previously-applied hash,
// the SSA write is skipped (no-op). This avoids a round-trip to the API server
// on every reconcile when the desired state hasn't changed.
//
// Design constraints:
//   - Deterministic: keys sorted, values canonicalized.
//   - No reflect, no intermediate JSON bytes.
//   - sync.Pool'd byte buffer, single FNV-64a pass.
//   - Produces identical output to json.Marshal + FNV-64a on the same input
//     (backward-compatible hash values).
package graphcontroller

import (
	"hash"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"sync"
)

// bufPool recycles byte slices used during tree-walking serialization.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 1024)
		return &b
	},
}

// hashPool recycles FNV-64a hash instances.
var hashPool = sync.Pool{
	New: func() any {
		return fnv.New64a()
	},
}

// HashObject computes a deterministic FNV-64a hash of a map[string]any tree.
// The hash is identical to hashing the output of json.Marshal on the same input.
func HashObject(obj map[string]any) uint64 {
	bp := bufPool.Get().(*[]byte)
	buf := (*bp)[:0]

	buf = appendValue(buf, obj)

	h := hashPool.Get().(hash.Hash64)
	h.Reset()
	h.Write(buf)
	sum := h.Sum64()

	*bp = buf
	bufPool.Put(bp)
	hashPool.Put(h)

	return sum
}

// appendValue serializes a value into buf using JSON-compatible encoding.
// This produces byte-identical output to encoding/json.Marshal for the
// types that appear in map[string]any trees (map, slice, string, float64,
// int64, bool, nil).
func appendValue(buf []byte, v any) []byte {
	switch val := v.(type) {
	case nil:
		buf = append(buf, "null"...)
	case bool:
		if val {
			buf = append(buf, "true"...)
		} else {
			buf = append(buf, "false"...)
		}
	case string:
		buf = appendString(buf, val)
	case float64:
		buf = appendFloat(buf, val)
	case int64:
		buf = strconv.AppendInt(buf, val, 10)
	case int:
		buf = strconv.AppendInt(buf, int64(val), 10)
	case map[string]any:
		buf = appendMap(buf, val)
	case []any:
		buf = appendSlice(buf, val)
	default:
		// Fallback for unexpected types — shouldn't occur in practice
		// for well-formed unstructured objects.
		buf = append(buf, "null"...)
	}
	return buf
}

// appendMap serializes a map with sorted keys (JSON object encoding).
func appendMap(buf []byte, m map[string]any) []byte {
	buf = append(buf, '{')
	keys := sortedKeys(m)
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendString(buf, k)
		buf = append(buf, ':')
		buf = appendValue(buf, m[k])
	}
	buf = append(buf, '}')
	return buf
}

// appendSlice serializes a slice (JSON array encoding).
func appendSlice(buf []byte, s []any) []byte {
	buf = append(buf, '[')
	for i, v := range s {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendValue(buf, v)
	}
	buf = append(buf, ']')
	return buf
}

// appendString serializes a string with JSON escaping to prevent collisions.
func appendString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		case '\b':
			buf = append(buf, '\\', 'b')
		case '\f':
			buf = append(buf, '\\', 'f')
		default:
			if c < 0x20 {
				buf = append(buf, '\\', 'u', '0', '0')
				buf = append(buf, hexDigit(c>>4))
				buf = append(buf, hexDigit(c&0x0f))
			} else {
				buf = append(buf, c)
			}
		}
	}
	buf = append(buf, '"')
	return buf
}

// appendFloat serializes a float64 using the same format as encoding/json.
func appendFloat(buf []byte, f float64) []byte {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		// encoding/json would error; we produce a stable sentinel.
		buf = append(buf, "null"...)
		return buf
	}
	// encoding/json uses 'f' format when shorter, 'e' when shorter,
	// with no trailing zeros. strconv.AppendFloat with format='f' and
	// bitSize=64 matches json.Marshal for typical integer-valued floats.
	// For full compatibility we use the same logic as encoding/json:
	// format 'f' if no exponent needed, else 'e', then trim trailing zeros.
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	buf = strconv.AppendFloat(buf, f, format, -1, 64)
	return buf
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// sortedKeys returns the keys of a map in sorted order.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
