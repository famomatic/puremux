package vorbis

import "testing"

func FuzzValidateCodecPrivate(f *testing.F) {
	f.Add(testHeaders(), 2, 48_000)
	f.Add([]byte{2, 255}, 2, 48_000)
	f.Fuzz(func(t *testing.T, data []byte, channels, sampleRate int) {
		_ = ValidateCodecPrivate(data, channels, sampleRate)
	})
}
