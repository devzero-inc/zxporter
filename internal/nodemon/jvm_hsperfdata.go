package nodemon

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const perfMagic uint32 = 0xcafec0c0

// readHsperfdata reads a JVM hsperfdata binary file and returns counters as a
// map of name → value (int64 for numeric, string for byte-array counters).
func readHsperfdata(path string) (map[string]any, error) {
	const maxHsperfBytes = 4 << 20 // 4MiB safety cap

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxHsperfBytes))
	if err != nil {
		return nil, err
	}
	return parseHsperfdata(data)
}

// parseHsperfdata parses the raw bytes of an hsperfdata file.
//
// Format reference: hotspot/share/services/perfMemory.hpp (OpenJDK).
// Prologue (32 bytes):
//
//	[0:4]  magic (jint, native byte order encodes endianness)
//	[4]    byte_order
//	[5]    major_version
//	[6]    minor_version
//	[7]    accessible
//	[8:12] used (jint)
//	[12:16] overflow (jint)
//	[16:24] mod_time_stamp (jlong)
//	[24:28] entry_offset (jint)
//	[28:32] num_entries (jint)
//
// Each entry header (20 bytes, relative to entry start):
//
//	[0:4]  entry_length
//	[4:8]  name_offset  (from entry start)
//	[8:12] vector_length (0 = scalar)
//	[12]   data_type ('J'=long, 'B'=byte/string, 'I'=int)
//	[13]   flags
//	[14]   data_units
//	[15]   data_variability
//	[16:20] data_offset (from entry start)
func parseHsperfdata(data []byte) (map[string]any, error) {
	if len(data) < 32 {
		return nil, fmt.Errorf("hsperfdata: file too short (%d bytes)", len(data))
	}

	// Detect byte order from magic.
	var order binary.ByteOrder
	if binary.BigEndian.Uint32(data[0:4]) == perfMagic {
		order = binary.BigEndian
	} else if binary.LittleEndian.Uint32(data[0:4]) == perfMagic {
		order = binary.LittleEndian
	} else {
		return nil, fmt.Errorf("hsperfdata: invalid magic bytes")
	}

	entryOffset := int(order.Uint32(data[24:28]))
	numEntries := int(order.Uint32(data[28:32]))
	if entryOffset < 32 || entryOffset >= len(data) {
		return nil, fmt.Errorf("hsperfdata: invalid entry_offset=%d len=%d", entryOffset, len(data))
	}
	// Guard against corrupted headers.
	if numEntries < 0 || numEntries > 100_000 {
		return nil, fmt.Errorf("hsperfdata: unreasonable num_entries=%d", numEntries)
	}

	result := make(map[string]any, minInt(numEntries, 2048))

	offset := entryOffset
	for i := 0; i < numEntries; i++ {
		if offset+20 > len(data) {
			break
		}
		entryStart := offset
		entryLength := int(order.Uint32(data[offset : offset+4]))
		if entryLength <= 0 {
			return nil, fmt.Errorf("hsperfdata: invalid entry_length=%d at entry=%d", entryLength, i)
		}
		entryEnd := entryStart + entryLength
		if entryEnd > len(data) {
			return nil, fmt.Errorf("hsperfdata: entry beyond eof entry=%d end=%d len=%d", i, entryEnd, len(data))
		}

		nameOffset := int(order.Uint32(data[offset+4 : offset+8]))
		vectorLength := int(order.Uint32(data[offset+8 : offset+12]))
		dataType := data[offset+12]
		dataOffset := int(order.Uint32(data[offset+16 : offset+20]))

		// Validate offsets are within the entry.
		if nameOffset < 0 || nameOffset >= entryLength {
			offset = entryEnd
			continue
		}
		if dataOffset < 0 || dataOffset >= entryLength {
			offset = entryEnd
			continue
		}

		// Read null-terminated name (bounded to this entry).
		nameStart := entryStart + nameOffset
		nameEnd := nameStart
		for nameEnd < entryEnd && data[nameEnd] != 0 {
			nameEnd++
		}
		if nameEnd <= nameStart {
			offset = entryEnd
			continue
		}
		name := string(data[nameStart:nameEnd])

		dataStart := entryStart + dataOffset

		switch dataType {
		case 'J': // long (int64)
			if dataStart+8 <= entryEnd {
				result[name] = int64(order.Uint64(data[dataStart : dataStart+8]))
			}
		case 'I': // int (int32)
			if dataStart+4 <= entryEnd {
				result[name] = int64(int32(order.Uint32(data[dataStart : dataStart+4])))
			}
		case 'B': // byte scalar or byte-array string
			if vectorLength > 0 {
				// Cap vector length to avoid pathological allocations on corrupt data.
				if vectorLength > 256<<10 {
					vectorLength = 256 << 10
				}
				maxEnd := dataStart + vectorLength
				if maxEnd > entryEnd {
					maxEnd = entryEnd
				}
				end := dataStart
				for end < maxEnd && data[end] != 0 {
					end++
				}
				result[name] = string(data[dataStart:end])
			} else if dataStart < entryEnd {
				result[name] = int64(data[dataStart])
			}
		default:
			// Unknown type: skip.
		}

		offset = entryEnd
	}

	return result, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// hsInt looks up an int64 counter.
func hsInt(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	i, ok := v.(int64)
	return i, ok
}

// hsStr looks up a string counter.
func hsStr(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
