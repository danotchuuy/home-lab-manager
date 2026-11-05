package webauthn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// A small CBOR (RFC 8949) decoder: enough for WebAuthn's attestation
// objects and COSE keys. Integers decode to int64, byte strings to []byte,
// text to string, arrays to []any and maps to map[any]any (keys int64 or
// string). Tags are read through; floats, undefined and indefinite lengths
// aren't used by WebAuthn and are refused.

var errCBOR = errors.New("malformed CBOR")

const (
	maxCBORDepth = 16
	maxCBORItems = 4096 // elements in one array or map
)

// decodeCBOR reads one item from data, returning it and what follows.
func decodeCBOR(data []byte) (any, []byte, error) {
	return decodeItem(data, 0)
}

func decodeHead(data []byte) (major byte, arg uint64, rest []byte, err error) {
	if len(data) == 0 {
		return 0, 0, nil, errCBOR
	}
	major, info := data[0]>>5, data[0]&0x1f
	data = data[1:]
	switch {
	case info < 24:
		return major, uint64(info), data, nil
	case info == 24:
		if len(data) < 1 {
			return 0, 0, nil, errCBOR
		}
		return major, uint64(data[0]), data[1:], nil
	case info == 25:
		if len(data) < 2 {
			return 0, 0, nil, errCBOR
		}
		return major, uint64(binary.BigEndian.Uint16(data)), data[2:], nil
	case info == 26:
		if len(data) < 4 {
			return 0, 0, nil, errCBOR
		}
		return major, uint64(binary.BigEndian.Uint32(data)), data[4:], nil
	case info == 27:
		if len(data) < 8 {
			return 0, 0, nil, errCBOR
		}
		return major, binary.BigEndian.Uint64(data), data[8:], nil
	}
	return 0, 0, nil, fmt.Errorf("%w: indefinite or reserved length", errCBOR)
}

func decodeItem(data []byte, depth int) (any, []byte, error) {
	if depth > maxCBORDepth {
		return nil, nil, fmt.Errorf("%w: nested too deeply", errCBOR)
	}
	major, arg, rest, err := decodeHead(data)
	if err != nil {
		return nil, nil, err
	}
	switch major {
	case 0: // unsigned integer
		if arg > math.MaxInt64 {
			return nil, nil, fmt.Errorf("%w: integer too large", errCBOR)
		}
		return int64(arg), rest, nil
	case 1: // negative integer: -1 - arg
		if arg > math.MaxInt64 {
			return nil, nil, fmt.Errorf("%w: integer too large", errCBOR)
		}
		return -1 - int64(arg), rest, nil
	case 2, 3: // byte string, text string
		if arg > uint64(len(rest)) {
			return nil, nil, fmt.Errorf("%w: string longer than the data", errCBOR)
		}
		b := rest[:arg]
		if major == 3 {
			return string(b), rest[arg:], nil
		}
		return bytes.Clone(b), rest[arg:], nil
	case 4: // array
		if arg > maxCBORItems {
			return nil, nil, fmt.Errorf("%w: array too long", errCBOR)
		}
		out := make([]any, 0, arg)
		for range arg {
			var v any
			if v, rest, err = decodeItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			out = append(out, v)
		}
		return out, rest, nil
	case 5: // map
		if arg > maxCBORItems {
			return nil, nil, fmt.Errorf("%w: map too long", errCBOR)
		}
		out := make(map[any]any, arg)
		for range arg {
			var k, v any
			if k, rest, err = decodeItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, fmt.Errorf("%w: map key of type %T", errCBOR, k)
			}
			if _, dup := out[k]; dup {
				return nil, nil, fmt.Errorf("%w: duplicate map key %v", errCBOR, k)
			}
			if v, rest, err = decodeItem(rest, depth+1); err != nil {
				return nil, nil, err
			}
			out[k] = v
		}
		return out, rest, nil
	case 6: // tag: the tagged item stands for itself here
		return decodeItem(rest, depth+1)
	case 7: // simple values
		switch arg {
		case 20:
			return false, rest, nil
		case 21:
			return true, rest, nil
		case 22:
			return nil, rest, nil
		}
		return nil, nil, fmt.Errorf("%w: unsupported simple value or float", errCBOR)
	}
	return nil, nil, errCBOR
}

// --- Encoding, for tests and for keeping keys ---

func appendHead(b []byte, major byte, n uint64) []byte {
	switch {
	case n < 24:
		return append(b, major<<5|byte(n))
	case n <= math.MaxUint8:
		return append(b, major<<5|24, byte(n))
	case n <= math.MaxUint16:
		return binary.BigEndian.AppendUint16(append(b, major<<5|25), uint16(n))
	case n <= math.MaxUint32:
		return binary.BigEndian.AppendUint32(append(b, major<<5|26), uint32(n))
	}
	return binary.BigEndian.AppendUint64(append(b, major<<5|27), n)
}

// encodeCBOR encodes the types decodeCBOR returns (and int, uint32), with
// map keys in no particular order. Tests use it to play an authenticator.
func encodeCBOR(v any) ([]byte, error) {
	return appendCBOR(nil, v)
}

func appendCBOR(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case int:
		return appendCBOR(b, int64(x))
	case uint32:
		return appendCBOR(b, int64(x))
	case int64:
		if x >= 0 {
			return appendHead(b, 0, uint64(x)), nil
		}
		return appendHead(b, 1, uint64(-1-x)), nil
	case []byte:
		return append(appendHead(b, 2, uint64(len(x))), x...), nil
	case string:
		return append(appendHead(b, 3, uint64(len(x))), x...), nil
	case []any:
		b = appendHead(b, 4, uint64(len(x)))
		var err error
		for _, e := range x {
			if b, err = appendCBOR(b, e); err != nil {
				return nil, err
			}
		}
		return b, nil
	case map[any]any:
		b = appendHead(b, 5, uint64(len(x)))
		var err error
		for k, e := range x {
			if b, err = appendCBOR(b, k); err != nil {
				return nil, err
			}
			if b, err = appendCBOR(b, e); err != nil {
				return nil, err
			}
		}
		return b, nil
	case bool:
		if x {
			return append(b, 0xf5), nil
		}
		return append(b, 0xf4), nil
	case nil:
		return append(b, 0xf6), nil
	}
	return nil, fmt.Errorf("can't encode %T as CBOR", v)
}
