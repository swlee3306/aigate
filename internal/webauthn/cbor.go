package webauthn

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Minimal CBOR (RFC 8949) decoder: only what an attestation object and a
// COSE key need. Definite lengths only; integers, byte/text strings, arrays
// and maps; map keys must be integers or text and must not repeat.

var errCBOR = errors.New("webauthn: malformed CBOR")

const maxCBORDepth = 8

type cborKind uint8

const (
	cborInt cborKind = iota + 1
	cborBytes
	cborText
	cborArray
	cborMap
)

type cborPair struct{ key, val cborValue }

type cborValue struct {
	kind cborKind
	neg  bool   // cborInt: value is -1 - u
	u    uint64 // cborInt magnitude
	b    []byte // cborBytes / cborText
	arr  []cborValue
	m    []cborPair
}

// isInt reports whether v is the integer i.
func (v cborValue) isInt(i int64) bool {
	if v.kind != cborInt {
		return false
	}
	if i < 0 {
		return v.neg && v.u == uint64(-1-i)
	}
	return !v.neg && v.u == uint64(i)
}

func (v cborValue) isText(s string) bool { return v.kind == cborText && string(v.b) == s }

func sameKey(a, b cborValue) bool {
	return a.kind == b.kind && a.neg == b.neg && a.u == b.u && string(a.b) == string(b.b)
}

// decodeCBOR decodes exactly one data item; trailing bytes are an error.
func decodeCBOR(data []byte) (cborValue, error) {
	v, rest, err := decodeCBORPrefix(data)
	if err != nil {
		return cborValue{}, err
	}
	if len(rest) != 0 {
		return cborValue{}, fmt.Errorf("%w: %d trailing bytes", errCBOR, len(rest))
	}
	return v, nil
}

// decodeCBORPrefix decodes one data item and returns the remaining bytes.
func decodeCBORPrefix(data []byte) (cborValue, []byte, error) {
	d := cborDecoder{data: data}
	v, err := d.item(1)
	if err != nil {
		return cborValue{}, nil, err
	}
	return v, d.data[d.off:], nil
}

type cborDecoder struct {
	data []byte
	off  int
}

func (d *cborDecoder) remaining() uint64 { return uint64(len(d.data) - d.off) }

func (d *cborDecoder) head() (major byte, arg uint64, err error) {
	if d.remaining() < 1 {
		return 0, 0, fmt.Errorf("%w: unexpected end", errCBOR)
	}
	ib := d.data[d.off]
	d.off++
	major, info := ib>>5, ib&0x1f
	var n int
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info == 24:
		n = 1
	case info == 25:
		n = 2
	case info == 26:
		n = 4
	case info == 27:
		n = 8
	default: // 28..30 reserved, 31 indefinite length / break
		return 0, 0, fmt.Errorf("%w: unsupported additional info %d", errCBOR, info)
	}
	if d.remaining() < uint64(n) {
		return 0, 0, fmt.Errorf("%w: unexpected end", errCBOR)
	}
	b := d.data[d.off : d.off+n]
	d.off += n
	switch n {
	case 1:
		arg = uint64(b[0])
	case 2:
		arg = uint64(binary.BigEndian.Uint16(b))
	case 4:
		arg = uint64(binary.BigEndian.Uint32(b))
	default:
		arg = binary.BigEndian.Uint64(b)
	}
	return major, arg, nil
}

func (d *cborDecoder) item(depth int) (cborValue, error) {
	major, arg, err := d.head()
	if err != nil {
		return cborValue{}, err
	}
	switch major {
	case 0:
		return cborValue{kind: cborInt, u: arg}, nil
	case 1:
		return cborValue{kind: cborInt, neg: true, u: arg}, nil
	case 2, 3:
		if arg > d.remaining() {
			return cborValue{}, fmt.Errorf("%w: string length %d exceeds input", errCBOR, arg)
		}
		b := d.data[d.off : d.off+int(arg)]
		d.off += int(arg)
		if major == 2 {
			return cborValue{kind: cborBytes, b: b}, nil
		}
		return cborValue{kind: cborText, b: b}, nil
	case 4, 5:
		if depth > maxCBORDepth {
			return cborValue{}, fmt.Errorf("%w: nesting deeper than %d", errCBOR, maxCBORDepth)
		}
		per := uint64(1) // each element needs at least one byte
		if major == 5 {
			per = 2
		}
		if arg > d.remaining()/per {
			return cborValue{}, fmt.Errorf("%w: element count %d exceeds input", errCBOR, arg)
		}
		if major == 4 {
			arr := make([]cborValue, 0, arg)
			for i := uint64(0); i < arg; i++ {
				v, err := d.item(depth + 1)
				if err != nil {
					return cborValue{}, err
				}
				arr = append(arr, v)
			}
			return cborValue{kind: cborArray, arr: arr}, nil
		}
		m := make([]cborPair, 0, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := d.item(depth + 1)
			if err != nil {
				return cborValue{}, err
			}
			if k.kind != cborInt && k.kind != cborText {
				return cborValue{}, fmt.Errorf("%w: map key must be an integer or text", errCBOR)
			}
			for _, p := range m {
				if sameKey(p.key, k) {
					return cborValue{}, fmt.Errorf("%w: duplicate map key", errCBOR)
				}
			}
			v, err := d.item(depth + 1)
			if err != nil {
				return cborValue{}, err
			}
			m = append(m, cborPair{k, v})
		}
		return cborValue{kind: cborMap, m: m}, nil
	default: // 6 tags, 7 simple values and floats
		return cborValue{}, fmt.Errorf("%w: unsupported major type %d", errCBOR, major)
	}
}
