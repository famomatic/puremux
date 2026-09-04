// Package vp9 validates and converts VP9 container configuration records
// without decoding compressed frames.
package vp9

import "errors"

var errInvalidConfig = errors.New("vp9: invalid codec configuration")

// Config describes the four VP9 features shared by the MP4 vpcC record and
// Matroska CodecPrivate feature metadata.
type Config struct {
	Profile           byte
	Level             byte
	BitDepth          byte
	ChromaSubsampling byte
}

// ValidateVPCC validates the complete payload of a version-1 vpcC box.
func ValidateVPCC(data []byte) error {
	_, err := parseVPCC(data)
	return err
}

// FeatureMetadataFromVPCC converts an MP4 vpcC payload to the VP9 feature TLV
// list registered for Matroska CodecPrivate.
func FeatureMetadataFromVPCC(data []byte) ([]byte, error) {
	config, err := parseVPCC(data)
	if err != nil {
		return nil, err
	}
	return []byte{
		1, 1, config.Profile,
		2, 1, config.Level,
		3, 1, config.BitDepth,
		4, 1, config.ChromaSubsampling,
	}, nil
}

// ValidateFeatureMetadata validates a Matroska VP9 CodecPrivate feature list.
// An empty list is valid because the Matroska mapping makes it optional.
func ValidateFeatureMetadata(data []byte) error {
	_, _, err := parseFeatureMetadata(data)
	return err
}

// VPCCFromFeatureMetadata converts a complete Matroska VP9 feature list to a
// version-1 vpcC payload. Matroska does not carry the remaining vpcC colour
// fields here, so the VP9 binding defaults (BT.709, limited range) are used.
func VPCCFromFeatureMetadata(data []byte) ([]byte, error) {
	config, present, err := parseFeatureMetadata(data)
	if err != nil || present != 0x0f {
		return nil, errInvalidConfig
	}
	packed := config.BitDepth<<4 | config.ChromaSubsampling<<1
	return []byte{1, 0, 0, 0, config.Profile, config.Level, packed, 1, 1, 1, 0, 0}, nil
}

func parseVPCC(data []byte) (Config, error) {
	if len(data) != 12 || data[0] != 1 || data[1] != 0 || data[2] != 0 || data[3] != 0 ||
		data[10] != 0 || data[11] != 0 {
		return Config{}, errInvalidConfig
	}
	config := Config{
		Profile:           data[4],
		Level:             data[5],
		BitDepth:          data[6] >> 4,
		ChromaSubsampling: data[6] >> 1 & 0x07,
	}
	if !validConfig(config) || data[9] == 0 && config.ChromaSubsampling != 3 {
		return Config{}, errInvalidConfig
	}
	return config, nil
}

func parseFeatureMetadata(data []byte) (Config, byte, error) {
	var config Config
	var present byte
	for offset := 0; offset < len(data); {
		if len(data)-offset < 2 {
			return Config{}, 0, errInvalidConfig
		}
		id, length := data[offset], int(data[offset+1])
		offset += 2
		if id&0x80 != 0 || id < 1 || id > 4 || length != 1 || length > len(data)-offset {
			return Config{}, 0, errInvalidConfig
		}
		mask := byte(1 << (id - 1))
		if present&mask != 0 {
			return Config{}, 0, errInvalidConfig
		}
		present |= mask
		value := data[offset]
		switch id {
		case 1:
			config.Profile = value
		case 2:
			config.Level = value
		case 3:
			config.BitDepth = value
		case 4:
			config.ChromaSubsampling = value
		}
		offset += length
	}
	if present&0x01 != 0 && config.Profile > 3 ||
		present&0x02 != 0 && !validLevel(config.Level) ||
		present&0x04 != 0 && config.BitDepth != 8 && config.BitDepth != 10 && config.BitDepth != 12 ||
		present&0x08 != 0 && config.ChromaSubsampling > 3 {
		return Config{}, 0, errInvalidConfig
	}
	if present == 0x0f && !validConfig(config) {
		return Config{}, 0, errInvalidConfig
	}
	return config, present, nil
}

func validConfig(config Config) bool {
	if config.Profile > 3 || !validLevel(config.Level) ||
		config.BitDepth != 8 && config.BitDepth != 10 && config.BitDepth != 12 ||
		config.ChromaSubsampling > 3 {
		return false
	}
	if config.Profile&1 == 0 {
		return config.ChromaSubsampling <= 1 &&
			(config.Profile != 0 || config.BitDepth == 8) &&
			(config.Profile != 2 || config.BitDepth == 10 || config.BitDepth == 12)
	}
	return config.ChromaSubsampling >= 2 &&
		(config.Profile != 1 || config.BitDepth == 8) &&
		(config.Profile != 3 || config.BitDepth == 10 || config.BitDepth == 12)
}

func validLevel(level byte) bool {
	switch level {
	case 0, 10, 11, 20, 21, 30, 31, 40, 41, 50, 51, 52, 60, 61, 62:
		return true
	default:
		return false
	}
}
