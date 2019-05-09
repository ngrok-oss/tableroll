package proto

import "fmt"

// This file implements a way to encode and decode a uint32 as json-ignorable
// whitespace. Since JSON allows 4 whitespace characters that will be ignored,
// we can encode 2 bits per whitespace character. This file implements that
// encoding.

// encodeVersion encodes a given uint32 as a json-ignorable sequence of whitespace
func encodeVersion(version uint32) []byte {
	result := []byte{}
	for version > 0 {
		nibble := version % 16
		version = version >> 4

		crumb1 := byte(nibble & 0x3)
		crumb2 := byte(nibble >> 2)
		result = append(result, []byte{encodeCrumb(crumb1), encodeCrumb(crumb2)}...) //, result...)
	}
	return result
}

// decodeVersion decodes a version from a json-ignorable sequence of whitespace
func decodeVersion(data []byte) (uint32, error) {
	var version uint32
	for i := len(data) - 1; i >= 0; i-- {
		crumb, err := decodeCrumb(data[i])
		if err != nil {
			return 0, err
		}
		version = version << 2
		version += uint32(crumb)
	}
	return version, nil
}

// encodeCrumb encodes a crumb (2 bits, half a nibble) into a json-ignorable
// whitespace character.
func encodeCrumb(crumb byte) byte {
	switch crumb {
	case 0:
		return ' '
	case 1:
		return '\t'
	case 2:
		return '\r'
	case 3:
		return '\n'
	}
	panic(fmt.Sprintf("byePair was not actually a pair of bytes: %x", crumb))
}

// decodeCrumb is the inverse operation of encodeCrumb.
func decodeCrumb(char byte) (byte, error) {
	switch char {
	case ' ':
		return 0, nil
	case '\t':
		return 1, nil
	case '\r':
		return 2, nil
	case '\n':
		return 3, nil
	}
	return 0, fmt.Errorf("char was not a whitespace encoding of a bytepair: %x", char)
}

func isJSONIgnorableWhitespace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}
