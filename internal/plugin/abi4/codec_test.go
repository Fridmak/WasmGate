package abi4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEnvelopeTableValidation(t *testing.T) {
	in := mustWrite(t, Event{Block: []byte("abcd"), State: []byte("efgh")})
	dst := make([]byte, 128)
	n, err := WriteOutput(dst, Output{State: []byte("abcd"), Mutation: []byte("efgh")})
	if err != nil {
		t.Fatal(err)
	}
	le := binary.LittleEndian
	for _, codec := range []struct {
		name  string
		valid []byte
		parse func([]byte) error
	}{
		{"IN", in, func(b []byte) error { _, err := Parse(b); return err }},
		{"OUT", dst[:n], func(b []byte) error { _, err := ParseOutput(b); return err }},
	} {
		t.Run(codec.name, func(t *testing.T) {
			if err := codec.parse(codec.valid); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name string
				edit func([]byte)
			}{
				{"stride", func(b []byte) { le.PutUint16(b[14:], 12) }},
				{"reserved", func(b []byte) { le.PutUint16(b[hdrLen+2:], 1) }},
				{"duplicate", func(b []byte) { le.PutUint16(b[hdrLen+secStride:], le.Uint16(b[hdrLen:])) }},
				{"overlap", func(b []byte) { le.PutUint32(b[hdrLen+secStride+4:], le.Uint32(b[hdrLen+4:])) }},
				{"misalignment", func(b []byte) { le.PutUint32(b[hdrLen+4:], le.Uint32(b[hdrLen+4:])+1) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					b := bytes.Clone(codec.valid)
					tc.edit(b)
					if err := codec.parse(b); err == nil {
						t.Fatalf("%s parser accepted invalid section table", codec.name)
					}
				})
			}
		})
	}
}
