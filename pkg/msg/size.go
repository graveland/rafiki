// SPDX-License-Identifier: Apache-2.0

package msg

import "unsafe"

// ApproxBytes estimates the heap a decoded Message holds: the block headers
// plus every string and byte payload they reference. It is an estimate for
// capacity metrics, not an accounting: allocator rounding, map overhead and
// shared backing arrays are ignored, so it is a floor of the decoded form's
// real footprint.
func (m Message) ApproxBytes() int64 {
	n := int64(unsafe.Sizeof(m))
	for _, b := range m.Content {
		n += blockBytes(b)
	}
	return n
}

func blockBytes(b Block) int64 {
	switch v := b.(type) {
	case Text:
		return int64(unsafe.Sizeof(v)) + int64(len(v.Text)+len(v.InjectedKind)+len(v.Ext.Raw))
	case *Text:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	case Thinking:
		return int64(unsafe.Sizeof(v)) + int64(len(v.Text)+len(v.Sig.Value)+len(v.Ext.Raw))
	case *Thinking:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	case ToolUse:
		return int64(unsafe.Sizeof(v)) + int64(len(v.ID)+len(v.Name)+len(v.Input)+len(v.Ext.Raw))
	case *ToolUse:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	case ToolResult:
		n := int64(unsafe.Sizeof(v)) + int64(len(v.ToolUseID)+len(v.Ext.Raw))
		for _, c := range v.Content {
			n += blockBytes(c)
		}
		return n
	case *ToolResult:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	case Image:
		return int64(unsafe.Sizeof(v)) + int64(len(v.MediaType)+len(v.Data)+len(v.Ext.Raw))
	case *Image:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	case Opaque:
		return int64(unsafe.Sizeof(v)) + int64(len(v.Raw))
	case *Opaque:
		if v == nil {
			return 0
		}
		return blockBytes(*v)
	}
	return 0
}
