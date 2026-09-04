package flac

import "testing"

func FuzzFLACParsers(f *testing.F) {
	f.Add(testStreamInfo())
	native := append([]byte("fLaC"), 0x80, 0, 0, 34)
	native = append(native, testStreamInfo()...)
	f.Add(native)
	f.Add([]byte{0xff, 0xf8, 0x89, 0x18, 0, 0})
	f.Add([]byte(nil))
	stream, err := ParseStreamInfo(testStreamInfo())
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseStreamInfo(data)
		_, _ = ParseFrameHeader(data, stream)
		_, _ = StreamInfoFromDFLA(data)
		_, _, _ = MatroskaCodecPrivate(data)
	})
}
