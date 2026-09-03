package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
)

func (rd *Reader) parseMvhd(r io.Reader, b box) error {
	payload, err := readBoundedPayload(r, b)
	if err != nil {
		return err
	}
	if len(payload) < 20 {
		return ErrCorrupt
	}
	if payload[0] == 1 {
		if len(payload) < 32 {
			return ErrCorrupt
		}
		rd.movieTimescale = binary.BigEndian.Uint32(payload[20:24])
		rd.duration = binary.BigEndian.Uint64(payload[24:32])
	} else if payload[0] == 0 {
		rd.movieTimescale = binary.BigEndian.Uint32(payload[12:16])
		rd.duration = uint64(binary.BigEndian.Uint32(payload[16:20]))
	} else {
		return ErrCorrupt
	}
	return nil
}

func (rd *Reader) parseTkhd(r io.Reader, b box, t *trackState) error {
	payload, err := readBoundedPayload(r, b)
	if err != nil {
		return err
	}
	if len(payload) < 16 {
		return ErrCorrupt
	}
	if payload[0] == 1 {
		if len(payload) < 24 {
			return ErrCorrupt
		}
		t.info.ID = binary.BigEndian.Uint32(payload[20:24])
	} else if payload[0] == 0 {
		t.info.ID = binary.BigEndian.Uint32(payload[12:16])
	} else {
		return ErrCorrupt
	}
	return nil
}

func (rd *Reader) parseEdts(r io.Reader, b box, t *trackState) error {
	payload, err := readBoundedPayload(r, b)
	if err != nil {
		return err
	}
	er := bytes.NewReader(payload)
	for er.Len() > 0 {
		child, err := readBox(er)
		if err != nil {
			return err
		}
		if child.typ != "elst" {
			if err := skipBox(er, child); err != nil {
				return err
			}
			continue
		}
		data, err := readBoundedPayload(er, child)
		if err != nil {
			return err
		}
		if len(data) < 8 {
			return ErrCorrupt
		}
		version := data[0]
		count := binary.BigEndian.Uint32(data[4:8])
		offset := 8
		for range count {
			if version == 0 {
				if len(data)-offset < 12 {
					return ErrCorrupt
				}
				segment := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
				mediaTime := int64(int32(binary.BigEndian.Uint32(data[offset+4 : offset+8])))
				if mediaTime == -1 {
					t.editLeadMovie += segment
				} else if !t.hasEditMediaTime {
					t.editMediaTime = mediaTime
					t.hasEditMediaTime = true
				}
				offset += 12
			} else if version == 1 {
				if len(data)-offset < 20 {
					return ErrCorrupt
				}
				segment := binary.BigEndian.Uint64(data[offset : offset+8])
				mediaTime := int64(binary.BigEndian.Uint64(data[offset+8 : offset+16]))
				if mediaTime == -1 {
					t.editLeadMovie += segment
				} else if !t.hasEditMediaTime {
					t.editMediaTime = mediaTime
					t.hasEditMediaTime = true
				}
				offset += 20
			} else {
				return ErrCorrupt
			}
		}
	}
	return nil
}

func (rd *Reader) parseMetadataContainer(r io.Reader, b box, fullBox bool) error {
	payload, err := readBoundedPayload(r, b)
	if err != nil {
		return err
	}
	if fullBox {
		if len(payload) < 4 {
			return ErrCorrupt
		}
		payload = payload[4:]
	}
	return rd.walkMetadata(payload)
}

func (rd *Reader) walkMetadata(data []byte) error {
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		b, err := readBox(r)
		if err != nil {
			return err
		}
		payload, err := readBoundedPayload(r, b)
		if err != nil {
			return err
		}
		switch b.typ {
		case "meta":
			if len(payload) < 4 {
				return ErrCorrupt
			}
			if err := rd.walkMetadata(payload[4:]); err != nil {
				return err
			}
		case "ilst":
			if err := rd.parseILST(payload); err != nil {
				return err
			}
		case "udta":
			if err := rd.walkMetadata(payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rd *Reader) parseILST(data []byte) error {
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		item, err := readBox(r)
		if err != nil {
			return err
		}
		payload, err := readBoundedPayload(r, item)
		if err != nil {
			return err
		}
		key := map[string]string{"\xa9nam": "title", "\xa9ART": "artist", "aART": "album_artist", "\xa9alb": "album", "\xa9day": "date", "\xa9gen": "genre"}[item.typ]
		if key == "" {
			continue
		}
		ir := bytes.NewReader(payload)
		valueBox, err := readBox(ir)
		if err != nil || valueBox.typ != "data" || valueBox.payload < 8 {
			continue
		}
		value, err := readBoundedPayload(ir, valueBox)
		if err != nil {
			return err
		}
		rd.metadata[key] = strings.TrimRight(string(value[8:]), "\x00")
	}
	return nil
}

func readBoundedPayload(r io.Reader, b box) ([]byte, error) {
	if b.payload < 0 || b.payload > 1<<30 {
		return nil, ErrCorrupt
	}
	payload := make([]byte, b.payload)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func decodeLanguage(packed uint16) string {
	if packed == 0 {
		return "und"
	}
	code := []byte{byte((packed>>10)&0x1f) + 0x60, byte((packed>>5)&0x1f) + 0x60, byte(packed&0x1f) + 0x60}
	for _, value := range code {
		if value < 'a' || value > 'z' {
			return "und"
		}
	}
	return string(code)
}

func (rd *Reader) MovieDuration() (uint64, uint32) { return rd.duration, rd.movieTimescale }

func (rd *Reader) Metadata() map[string]string {
	out := make(map[string]string, len(rd.metadata))
	for key, value := range rd.metadata {
		out[key] = value
	}
	return out
}
