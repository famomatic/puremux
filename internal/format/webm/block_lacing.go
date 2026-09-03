package webm

import (
	"errors"
	"math"

	"github.com/famomatic/puremux/internal/format/ebml"
)

type parsedBlock struct {
	track       int
	relTimecode int16
	flags       byte
	frames      []blockFrame
}

type blockFrame struct {
	data   []byte
	offset int
}

// parseBlockPayload parses the Matroska Block/SimpleBlock header and all four
// lacing modes. Returned frame slices alias payload.
func parseBlockPayload(payload []byte) (parsedBlock, error) {
	if len(payload) == 0 {
		return parsedBlock{}, errors.New("webm: block is empty")
	}
	w := ebml.VINTWidth(payload[0])
	if w == 0 || w > 8 || len(payload) < w+3 {
		return parsedBlock{}, errors.New("webm: truncated block header")
	}
	track, _, err := ebml.DecodeVINT(payload[:w])
	if err != nil || track == 0 || track > math.MaxInt {
		return parsedBlock{}, errors.New("webm: invalid block track number")
	}
	b := parsedBlock{
		track:       int(track),
		relTimecode: int16(uint16(payload[w])<<8 | uint16(payload[w+1])),
		flags:       payload[w+2],
	}
	body := payload[w+3:]
	bodyOffset := w + 3
	lacing := (b.flags >> 1) & 3
	if lacing == 0 {
		b.frames = []blockFrame{{data: body, offset: bodyOffset}}
		return b, nil
	}
	if len(body) == 0 {
		return parsedBlock{}, errors.New("webm: truncated lace header")
	}
	frameCount := int(body[0]) + 1
	if frameCount < 2 || frameCount > 256 {
		return parsedBlock{}, errors.New("webm: invalid lace frame count")
	}
	body = body[1:]
	bodyOffset++

	var sizes []int64
	switch lacing {
	case 1:
		sizes = make([]int64, 0, frameCount)
		for i := 0; i < frameCount-1; i++ {
			var size int64
			for {
				if len(body) == 0 {
					return parsedBlock{}, errors.New("webm: truncated Xiph lace size")
				}
				v := body[0]
				body = body[1:]
				bodyOffset++
				size += int64(v)
				if v != 255 {
					break
				}
			}
			sizes = append(sizes, size)
		}
	case 2:
		if len(body)%frameCount != 0 {
			return parsedBlock{}, errors.New("webm: fixed lace payload is not divisible by frame count")
		}
		each := int64(len(body) / frameCount)
		sizes = make([]int64, frameCount-1)
		for i := range sizes {
			sizes[i] = each
		}
	case 3:
		first, width, err := readLaceVINT(body)
		if err != nil || first > math.MaxInt64 {
			return parsedBlock{}, errors.New("webm: invalid EBML lace first size")
		}
		body = body[width:]
		bodyOffset += width
		sizes = append(sizes, int64(first))
		for i := 1; i < frameCount-1; i++ {
			u, n, err := readLaceVINT(body)
			if err != nil {
				return parsedBlock{}, errors.New("webm: truncated EBML lace delta")
			}
			body = body[n:]
			bodyOffset += n
			bits := uint(7 * n)
			bias := (uint64(1) << (bits - 1)) - 1
			delta := int64(u) - int64(bias)
			next := sizes[len(sizes)-1] + delta
			if next < 0 {
				return parsedBlock{}, errors.New("webm: negative EBML lace size")
			}
			sizes = append(sizes, next)
		}
	}

	remaining := int64(len(body))
	for _, size := range sizes {
		if size < 0 || size > remaining {
			return parsedBlock{}, errors.New("webm: lace sizes overrun block")
		}
		remaining -= size
	}
	sizes = append(sizes, remaining)
	b.frames = make([]blockFrame, 0, frameCount)
	pos := 0
	for _, size := range sizes {
		end := pos + int(size)
		b.frames = append(b.frames, blockFrame{data: body[pos:end], offset: bodyOffset + pos})
		pos = end
	}
	return b, nil
}

func readLaceVINT(data []byte) (uint64, int, error) {
	if len(data) == 0 {
		return 0, 0, errors.New("webm: missing lace VINT")
	}
	w := ebml.VINTWidth(data[0])
	if w == 0 || w > 8 || len(data) < w {
		return 0, 0, errors.New("webm: truncated lace VINT")
	}
	v, _, err := ebml.DecodeVINT(data[:w])
	return v, w, err
}
