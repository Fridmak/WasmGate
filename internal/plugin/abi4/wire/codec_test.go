package wire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

const (
	testInMagic  = 0x57473445
	testOutMagic = 0x5747344f
)

func validTable(magic uint32) []byte {
	b := make([]byte, 64)
	WriteEnvelopeHeader(b, magic, 64, 2, 56, 8)
	WriteSection(b, 0, SectionBlob, 56, 4, 0)
	WriteSection(b, 1, SectionState, 60, 4, 0)
	return b
}

func TestSectionTableValidation(t *testing.T) {
	for _, magic := range []uint32{testInMagic, testOutMagic} {
		t.Run("valid", func(t *testing.T) {
			if _, ok := ReadSectionTable(validTable(magic), magic); !ok {
				t.Fatal("valid section table rejected")
			}
		})

		for _, tc := range []struct {
			name string
			edit func([]byte)
		}{
			{"stride", func(b []byte) { binary.LittleEndian.PutUint16(b[14:], 12) }},
			{"reserved", func(b []byte) { binary.LittleEndian.PutUint16(b[HeaderLen+2:], 1) }},
			{"duplicate", func(b []byte) {
				binary.LittleEndian.PutUint16(b[HeaderLen+SectionStride:], binary.LittleEndian.Uint16(b[HeaderLen:]))
			}},
			{"overlap", func(b []byte) {
				binary.LittleEndian.PutUint32(b[HeaderLen+SectionStride+4:], binary.LittleEndian.Uint32(b[HeaderLen+4:]))
			}},
			{"misalignment", func(b []byte) {
				binary.LittleEndian.PutUint32(b[HeaderLen+4:], binary.LittleEndian.Uint32(b[HeaderLen+4:])+1)
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				b := bytes.Clone(validTable(magic))
				tc.edit(b)
				if _, ok := ReadSectionTable(b, magic); ok {
					t.Fatalf("accepted invalid section table: %s", tc.name)
				}
			})
		}
	}
}

// TestEnvelopeTableValidation keeps the pre-decomposition test name while the
// shared section-table codec now owns the validation boundary.
func TestEnvelopeTableValidation(t *testing.T) {
	TestSectionTableValidation(t)
}
