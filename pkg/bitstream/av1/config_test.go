package av1

import "testing"

func TestValidateConfigBoundaries(t *testing.T) {
	// marker=1/version=1 produces 10000001b (0x81), MSB-first. OBU
	// 0x12 is a Temporal Delimiter with has_size_field=1 and a zero-byte
	// payload, encoded by the one-byte unsigned LEB128 value 0x00.
	for _, test := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{name: "minimum", data: []byte{0x81, 0, 0, 0}, ok: true},
		{name: "presentation delay", data: []byte{0x81, 0, 0, 0x1f}, ok: true},
		{name: "temporal delimiter OBU", data: []byte{0x81, 0, 0, 0, 0x12, 0}, ok: true},
		{name: "nil", data: nil},
		{name: "truncated", data: []byte{0x81, 0, 0}},
		{name: "marker", data: []byte{0x01, 0, 0, 0}},
		{name: "version", data: []byte{0x82, 0, 0, 0}},
		{name: "reserved", data: []byte{0x81, 0, 0, 0x20}},
		{name: "delay absent reserved nibble", data: []byte{0x81, 0, 0, 0x01}},
		{name: "reserved profile", data: []byte{0x81, 0x60, 0, 0}},
		{name: "higher reserved profile", data: []byte{0x81, 0x80, 0, 0}},
		{name: "reserved level", data: []byte{0x81, 24, 0, 0}},
		{name: "OBU forbidden bit", data: []byte{0x81, 0, 0, 0, 0x92, 0}},
		{name: "OBU reserved bit", data: []byte{0x81, 0, 0, 0, 0x13, 0}},
		{name: "OBU missing size field", data: []byte{0x81, 0, 0, 0, 0x10}},
		{name: "OBU extension truncated", data: []byte{0x81, 0, 0, 0, 0x16}},
		{name: "OBU extension reserved", data: []byte{0x81, 0, 0, 0, 0x16, 1, 0}},
		{name: "LEB128 truncated", data: []byte{0x81, 0, 0, 0, 0x12, 0x80}},
		{name: "payload overrun", data: []byte{0x81, 0, 0, 0, 0x12, 1}},
		{name: "reserved OBU type", data: []byte{0x81, 0, 0, 0, 0x4a, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidateConfig(test.data) == nil; got != test.ok {
				t.Fatalf("ValidateConfig(%x) success = %v, want %v", test.data, got, test.ok)
			}
		})
	}
}

func TestHasSequenceHeader(t *testing.T) {
	// AV1 section 5.5.1, MSB-first reduced-still-picture Sequence Header:
	// profile 0, still_picture=1, reduced header=1, level 0, 1x1 maximum,
	// disabled tools, 8-bit monochrome limited-range colour, no film grain,
	// then trailing_one_bit => payload 18 00 00 11. av1C byte 2 mirrors the
	// monochrome and 4:0:0 subsampling fields as 00 011 100b = 0x1c.
	config := []byte{0x81, 0x00, 0x1c, 0x00, 0x0a, 0x04, 0x18, 0x00, 0x00, 0x11}
	has, err := HasSequenceHeader(config)
	if err != nil || !has {
		t.Fatalf("HasSequenceHeader() = %v, %v", has, err)
	}
	// A Sequence Header must be first and unique in configOBUs.
	for _, malformed := range [][]byte{
		append([]byte{0x81, 0, 0, 0, 0x12, 0}, config[4:]...),
		append(append([]byte(nil), config...), config[4:]...),
		{0x81, 0, 0, 0, 0x0a, 0},
	} {
		if _, err := HasSequenceHeader(malformed); err == nil {
			t.Fatalf("malformed config accepted: %x", malformed)
		}
	}
}
