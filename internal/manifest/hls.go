// Package manifest parses adaptive-stream manifests into bounded segment
// descriptions. It never parses or transforms codec payloads.
package manifest

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type ByteRange struct {
	Offset int64
	Length int64
	Valid  bool
}

type HLSKey struct {
	Method string
	URI    string
	IV     [16]byte
	HasIV  bool
}

type HLSMap struct {
	URI   string
	Range ByteRange
}

type HLSSegment struct {
	Sequence      uint64
	URI           string
	Duration      time.Duration
	Title         string
	Range         ByteRange
	Map           *HLSMap
	Key           *HLSKey
	Discontinuity bool
}

type HLSVariant struct {
	URI        string
	Bandwidth  int64
	Codecs     string
	AudioGroup string
}

type HLSRendition struct {
	Type       string
	GroupID    string
	Name       string
	URI        string
	Language   string
	Default    bool
	Autoselect bool
}

type HLSPlaylist struct {
	Master         bool
	Variants       []HLSVariant
	Renditions     []HLSRendition
	Segments       []HLSSegment
	MediaSequence  uint64
	TargetDuration time.Duration
	EndList        bool
}

func ParseHLS(base *url.URL, data []byte, maxEntries int) (HLSPlaylist, error) {
	if base == nil {
		return HLSPlaylist{}, errors.New("hls: nil base URL")
	}
	if maxEntries <= 0 {
		maxEntries = 100000
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	lineNumber := 0
	first := true
	var playlist HLSPlaylist
	var pendingVariant *HLSVariant
	var pendingDuration time.Duration
	var pendingTitle string
	var pendingRange ByteRange
	var pendingRangeImplicit bool
	var currentMap *HLSMap
	var currentKey *HLSKey
	var discontinuity bool
	var previousRangeEnd int64
	var previousRangeURI string
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			line = strings.TrimPrefix(line, "\ufeff")
			if line != "#EXTM3U" {
				return HLSPlaylist{}, errors.New("hls: missing EXTM3U header")
			}
			continue
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return HLSPlaylist{}, fmt.Errorf("hls: line %d: %w", lineNumber, err)
			}
			bandwidth, err := strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			if err != nil || bandwidth < 0 {
				return HLSPlaylist{}, errors.New("hls: variant missing valid BANDWIDTH")
			}
			pendingVariant = &HLSVariant{Bandwidth: bandwidth, Codecs: attrs["CODECS"], AudioGroup: attrs["AUDIO"]}
			playlist.Master = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if err != nil {
				return HLSPlaylist{}, err
			}
			r := HLSRendition{Type: attrs["TYPE"], GroupID: attrs["GROUP-ID"], Name: attrs["NAME"], Language: attrs["LANGUAGE"], Default: yes(attrs["DEFAULT"]), Autoselect: yes(attrs["AUTOSELECT"])}
			if attrs["URI"] != "" {
				r.URI, err = resolve(base, attrs["URI"])
				if err != nil {
					return HLSPlaylist{}, err
				}
			}
			playlist.Renditions = append(playlist.Renditions, r)
			playlist.Master = true
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			value, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			if err != nil {
				return HLSPlaylist{}, errors.New("hls: invalid media sequence")
			}
			playlist.MediaSequence = value
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			seconds, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")), 10, 32)
			if err != nil {
				return HLSPlaylist{}, errors.New("hls: invalid target duration")
			}
			playlist.TargetDuration = time.Duration(seconds) * time.Second
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			parts := strings.SplitN(strings.TrimPrefix(line, "#EXTINF:"), ",", 2)
			seconds, err := strconv.ParseFloat(parts[0], 64)
			if err != nil || seconds < 0 {
				return HLSPlaylist{}, errors.New("hls: invalid segment duration")
			}
			pendingDuration = time.Duration(seconds * float64(time.Second))
			if len(parts) == 2 {
				pendingTitle = parts[1]
			}
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-BYTERANGE:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
			var err error
			pendingRange, err = parseByteRange(value, previousRangeEnd)
			if err != nil {
				return HLSPlaylist{}, err
			}
			pendingRangeImplicit = !strings.Contains(value, "@")
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-MAP:") {
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if err != nil || attrs["URI"] == "" {
				return HLSPlaylist{}, errors.New("hls: invalid EXT-X-MAP")
			}
			uri, err := resolve(base, attrs["URI"])
			if err != nil {
				return HLSPlaylist{}, err
			}
			mapping := &HLSMap{URI: uri}
			if attrs["BYTERANGE"] != "" {
				mapping.Range, err = parseByteRange(attrs["BYTERANGE"], 0)
				if err != nil {
					return HLSPlaylist{}, err
				}
			}
			currentMap = mapping
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			if err != nil {
				return HLSPlaylist{}, err
			}
			method := attrs["METHOD"]
			if method == "NONE" {
				currentKey = nil
				continue
			}
			if method != "AES-128" || attrs["URI"] == "" {
				return HLSPlaylist{}, errors.New("hls: unsupported encryption method")
			}
			uri, err := resolve(base, attrs["URI"])
			if err != nil {
				return HLSPlaylist{}, err
			}
			key := &HLSKey{Method: method, URI: uri}
			if iv := attrs["IV"]; iv != "" {
				iv = strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X")
				if len(iv) == 0 || len(iv) > 32 {
					return HLSPlaylist{}, errors.New("hls: invalid AES IV")
				}
				if len(iv)%2 != 0 {
					iv = "0" + iv
				}
				decoded, err := hex.DecodeString(iv)
				if err != nil {
					return HLSPlaylist{}, errors.New("hls: invalid AES IV")
				}
				copy(key.IV[16-len(decoded):], decoded)
				key.HasIV = true
			}
			currentKey = key
			continue
		}
		if line == "#EXT-X-DISCONTINUITY" {
			discontinuity = true
			continue
		}
		if line == "#EXT-X-ENDLIST" {
			playlist.EndList = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		uri, err := resolve(base, line)
		if err != nil {
			return HLSPlaylist{}, err
		}
		if pendingVariant != nil {
			pendingVariant.URI = uri
			playlist.Variants = append(playlist.Variants, *pendingVariant)
			pendingVariant = nil
			if len(playlist.Variants) > maxEntries {
				return HLSPlaylist{}, errors.New("hls: too many variants")
			}
			continue
		}
		if len(playlist.Segments) >= maxEntries {
			return HLSPlaylist{}, errors.New("hls: too many segments")
		}
		if pendingRange.Valid && pendingRangeImplicit && (previousRangeURI == "" || previousRangeURI != uri) {
			return HLSPlaylist{}, errors.New("hls: implicit byte range has no preceding range on the same resource")
		}
		segment := HLSSegment{Sequence: playlist.MediaSequence + uint64(len(playlist.Segments)), URI: uri, Duration: pendingDuration, Title: pendingTitle, Range: pendingRange, Discontinuity: discontinuity}
		if currentMap != nil {
			copyMap := *currentMap
			segment.Map = &copyMap
		}
		if currentKey != nil {
			copyKey := *currentKey
			segment.Key = &copyKey
		}
		playlist.Segments = append(playlist.Segments, segment)
		if pendingRange.Valid {
			previousRangeEnd = pendingRange.Offset + pendingRange.Length
			previousRangeURI = uri
		}
		pendingDuration, pendingTitle, pendingRange, pendingRangeImplicit, discontinuity = 0, "", ByteRange{}, false, false
	}
	if err := scanner.Err(); err != nil {
		return HLSPlaylist{}, err
	}
	if first || pendingVariant != nil {
		return HLSPlaylist{}, errors.New("hls: truncated playlist")
	}
	if playlist.Master && len(playlist.Variants) == 0 && len(playlist.Renditions) == 0 {
		return HLSPlaylist{}, errors.New("hls: empty master playlist")
	}
	if !playlist.Master && len(playlist.Segments) == 0 {
		return HLSPlaylist{}, errors.New("hls: empty media playlist")
	}
	return playlist, nil
}

func parseAttributes(text string) (map[string]string, error) {
	out := make(map[string]string)
	for len(text) > 0 {
		equal := strings.IndexByte(text, '=')
		if equal <= 0 {
			return nil, errors.New("invalid attribute list")
		}
		key := strings.TrimSpace(text[:equal])
		text = text[equal+1:]
		var value string
		if strings.HasPrefix(text, "\"") {
			end := strings.IndexByte(text[1:], '"')
			if end < 0 {
				return nil, errors.New("unterminated quoted attribute")
			}
			value = text[1 : end+1]
			text = text[end+2:]
		} else if comma := strings.IndexByte(text, ','); comma >= 0 {
			value, text = text[:comma], text[comma:]
		} else {
			value, text = text, ""
		}
		out[key] = value
		if strings.HasPrefix(text, ",") {
			text = strings.TrimSpace(text[1:])
		} else if text != "" {
			return nil, errors.New("invalid attribute separator")
		}
	}
	return out, nil
}

func parseByteRange(value string, implicitOffset int64) (ByteRange, error) {
	parts := strings.Split(value, "@")
	if len(parts) > 2 {
		return ByteRange{}, errors.New("hls: invalid byte range")
	}
	length, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || length <= 0 {
		return ByteRange{}, errors.New("hls: invalid byte range length")
	}
	offset := implicitOffset
	if len(parts) == 2 {
		offset, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || offset < 0 {
			return ByteRange{}, errors.New("hls: invalid byte range offset")
		}
	}
	if offset > (1<<63-1)-length {
		return ByteRange{}, errors.New("hls: byte range overflow")
	}
	return ByteRange{Offset: offset, Length: length, Valid: true}, nil
}

func resolve(base *url.URL, reference string) (string, error) {
	ref, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", errors.New("hls: only HTTP(S) resources are supported")
	}
	return resolved.String(), nil
}

func yes(value string) bool { return strings.EqualFold(value, "YES") }
