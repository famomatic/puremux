package vorbis

import (
	"encoding/binary"
	"testing"
)

func testHeaders() []byte {
	identification := make([]byte, 30)
	identification[0] = 1
	copy(identification[1:7], "vorbis")
	identification[11] = 2
	binary.LittleEndian.PutUint32(identification[12:16], 48_000)
	identification[28], identification[29] = 0xa6, 1
	comment := append([]byte{3}, []byte("vorbis")...)
	comment = append(comment, make([]byte, 8)...)
	comment = append(comment, 1)
	setup := append(append([]byte{5}, []byte("vorbis")...), 0)
	data := []byte{2, byte(len(identification)), byte(len(comment))}
	data = append(data, identification...)
	data = append(data, comment...)
	return append(data, setup...)
}

func TestValidateCodecPrivateBoundaries(t *testing.T) {
	valid := testHeaders()
	badFraming := append([]byte(nil), valid...)
	badFraming[3+29] = 0
	badCommentLength := append([]byte(nil), valid...)
	commentOffset := 3 + 30
	binary.LittleEndian.PutUint32(badCommentLength[commentOffset+7:commentOffset+11], ^uint32(0))
	for _, test := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{name: "valid", data: valid, ok: true},
		{name: "nil", data: nil},
		{name: "lacing overrun", data: []byte{2, 255}},
		{name: "truncated setup", data: valid[:len(valid)-2]},
		{name: "bad framing", data: badFraming},
		{name: "comment length overrun", data: badCommentLength},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidateCodecPrivate(test.data, 2, 48_000) == nil; got != test.ok {
				t.Fatalf("ValidateCodecPrivate success = %v, want %v", got, test.ok)
			}
		})
	}
	if ValidateCodecPrivate(valid, 1, 48_000) == nil || ValidateCodecPrivate(valid, 2, 44_100) == nil {
		t.Fatal("track property mismatch accepted")
	}
}
