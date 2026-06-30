package webm

// Element IDs for WebM/Matroska (RFC 8794 + Matroska spec).
// These IDs include their marker bit (see ebml.EncodeElementID).
const (
	idEBML            uint32 = 0x1A45DFA3
	idEBMLVersion     uint32 = 0x4286
	idEBMLReadVersion uint32 = 0x42F7
	idEBMLMaxIDLength uint32 = 0x42F2
	idEBMLMaxSizeLen  uint32 = 0x42F3
	idDocType         uint32 = 0x4282
	idDocTypeVersion  uint32 = 0x4287
	idDocTypeReadVer  uint32 = 0x4285

	idSegment    uint32 = 0x18538067
	idSeekHead   uint32 = 0x114D9B74
	idSeek       uint32 = 0x4DBB
	idSeekID     uint32 = 0x53AB
	idSeekPosition uint32 = 0x53AC
	idInfo       uint32 = 0x1549A966
	idTimestampScale uint32 = 0x2AD7B1
	idDuration   uint32 = 0x4489
	idDateUTC    uint32 = 0x4461
	idMuxingApp  uint32 = 0x4D80
	idWritingApp uint32 = 0x5741

	idCluster     uint32 = 0x1F43B675
	idTimestamp   uint32 = 0xE7
	idSimpleBlock uint32 = 0xA3
	idBlockGroup  uint32 = 0xA0
	idBlock       uint32 = 0xA1

	idTracks     uint32 = 0x1654AE6B
	idTrackEntry uint32 = 0xAE
	idTrackNumber uint32 = 0xD7
	idTrackUID   uint32 = 0x73C5
	idTrackType  uint32 = 0x83
	idFlagLacing uint32 = 0x9C
	idCodecID    uint32 = 0x86
	idCodecPrivate uint32 = 0x63A2
	idVideo      uint32 = 0xE0
	idPixelWidth uint32 = 0xB0
	idPixelHeight uint32 = 0xBA
	idAudio      uint32 = 0xE1
	idChannels   uint32 = 0x9F
	idSamplingFrequency uint32 = 0xB5

	idCues      uint32 = 0x1C53BB6B
	idCuePoint  uint32 = 0xBB
	idCueTime   uint32 = 0xB3
	idCueTrackPositions uint32 = 0xB7
	idCueTrack  uint32 = 0xF7
	idCueClusterPosition uint32 = 0xF1
)

// WebM codec IDs (DocType/CodecID string values).
const (
	codecIDVP8  = "V_VP8"
	codecIDVP9  = "V_VP9"
	codecIDAV1  = "V_AV1"
	codecIDOpus   = "A_OPUS"
	codecIDVorbis = "A_VORBIS"
	codecIDFLAC   = "A_FLAC"
	codecIDH264   = "V_MPEG4/ISO/AVC"
	codecIDHEVC   = "V_MPEGH/ISO/SHEVC"
)

// TrackType values (Matroska spec).
const (
	trackTypeVideo uint64 = 1
	trackTypeAudio uint64 = 2
)
