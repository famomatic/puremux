# lavalink-go 소비 경로 기준 puremux 감사

2026-09-05 · Codex `/root`

## 2026-09-06 수정 결과

아래 최초 감사는 수정 전 기록이다. 현재 양쪽 작업 트리에 다음 수정과 회귀 테스트를 반영했다.

| 항목 | 반영 결과 |
| --- | --- |
| 1 | ReadAheadBytes=-1을 소비자에 적용해 spool의 추가 32 KiB 대기를 제거 |
| 2 | fMP4는 첫 moof까지만 초기화하고 다음 fragment를 재생 중 파싱 |
| 3 | Ogg는 OpusHead/Tags로 초기화, 재생 중 검증하고 EOS에서 길이 확정 |
| 4 | 공개 opus.PacketSamples로 통일, 120 ms 초과 거부 |
| 5 | 6초 요청 context를 실제 탐색 I/O까지 전파, 실패 뒤 read epoch 복원 |
| 6 | 새 라이브 세그먼트가 없는 경우 취소 가능한 대기 후 동일 demuxer 재시도 |
| 7 | 이전 Opus 패킷의 실제 끝을 기준으로 gap 계산 |
| 8 | 설정·trim 기반 passthrough 판정, FFmpeg skip-samples side data 전달 |
| 9 | 선택한 audio 시간 단위와 backward 탐색, audible 목표와 decoder preroll 분리 |
| 10 | 최신 Info/DurationKnown/metadata 조회 |
| 11 | Range 미지원·unknown-length HTTP의 순차 MPEG-TS 경로 연결 |
| 12 | 오류 체인 보존, MaxInitBytes/ErrInitLimit/OnOpen 진단 추가 |

DASH의 불가능한 time.Duration 상한 검사(SA4003)도 제거했다. puremux 전체
Staticcheck, 비CGO 테스트·vet·빌드·race 검사가 통과했다. lavalink-go 전체
비CGO 테스트·vet와 CGO 전체 race 검사도 통과했다. 실제 libavcodec 검사에서는
960-sample Opus 패킷의 앞 또는 뒤 10 ms를 버리면 480 samples가 출력됐다.

이 수정은 puremux v0.2.3 릴리스에 포함한다. 소비자는 공개 v0.2.3 의존성으로
전환하며, 로컬 go.work를 끈 상태로 배포된 모듈의 통합 검증을 수행한다.

남는 계약상 한계: 명시적 탐색은 아직 전체 인덱스를 읽을 수 있으며 context로
시간을 제한한다. 순차 WebM/Ogg에는 rewind 가능한 spool이 필요하다. gain,
pre-skip 또는 부분 패킷 trim이 필요한 Opus는 codec backend가 필요하다.
실제 YouTube/Discord 송출·청취를 검증한 것은 아니다.

## 범위와 검증

다운로드 spool → HTTPSource → Open → 패킷 → seek/recovery → Opus 송출 경로를 조사했다. 소비 프로젝트는 puremux v0.2.2를 사용하고, 현재 puremux 작업 트리에는 미배포 WebM 지연 초기화 수정이 있다.

- lavalink-go audio/httpproxy/player의 CGO_ENABLED=0 기존 테스트가 통과했다.
- 임시 modfile로 현재 로컬 puremux를 연결해도 같은 세 패키지가 통과했다.
- 추가 overlay 테스트 네 개는 의도한 계약을 assertion으로 작성했으며 현재 실패한다. 아래 실행 재현의 증거이며 수정 완료 테스트가 아니다.
- 테스트 소스/overlay/로그/임시 modfile: `F:/cache/puremux-lavalink-audit-20260905/`.
- 이번 조사에서 양쪽 제품 코드와 소비자의 go.mod/go.sum은 수정하지 않았다. 실제 YouTube/Discord 세션, CGO codec/filter 동작, lavalink-go 전체 테스트를 검증한 것은 아니다.

## 실행으로 재현한 결함

### 1. P1 — 32 KiB HTTP read-ahead가 spool 시작을 다시 막는다

puremux `pkg/media/source_http.go:102,117,294`; 소비자 `internal/audio/media.go:129`, `internal/httpproxy/stream_proxy.go:577`.

첫 1-byte Read도 32 KiB Range 전체를 io.ReadFull로 채운 뒤 반환한다. spool이 도착한 prefix를 즉시 flush해도 남은 다운로드를 기다린다. v0.2.2의 작은 Range 요청 감소 최적화가 이 경로에서는 역효과다.

재현: 전체 크기 65,536인 서버가 요청의 첫 128 bytes를 즉시 보내고 나머지는 대기하도록 했다. 1-byte probe는 성공하지만 다음 1-byte ReadContext는 n=0, context deadline exceeded로 끝났다.

수정: ReadAheadBytes/저지연 정책 또는 도착한 prefix를 반환하며 같은 응답을 이어 읽는 순차 Read. ReaderAt의 정확한 Range/SourceChanged 계약은 유지해야 한다.

### 2. P1 — fMP4 초기화도 뒤쪽 fragment를 요구한다

puremux `internal/format/mp4/reader.go:353`.

mdat payload를 seek로 건너뛰더라도 parse는 끝까지 모든 moof를 읽고 정렬한다. init+첫 fragment만 준비된 spool은 다음 moof에서 기다린다. 소비자에는 fragmented MP4 대응과 DASH fMP4 fixture가 이미 있다.

재현: 사양 기반 AAC 설정으로 두 fragment를 만들고 init+첫 fragment까지만 허용했다. Open은 tail not downloaded로 실패했다.

수정: init moov+첫 moof에서 시작하고 나머지는 ReadPacket 중 파싱한다. Progressive MP4의 moov-at-end는 원본 배치 때문에 별도 Range 접근/다운로드가 필요하므로 같은 보장을 해서는 안 된다.

### 3. P1 — Ogg Opus도 전체 page를 훑고 열린다

puremux `internal/format/ogg/reader.go:306,329`.

OpusHead/Tags와 첫 audio page가 있어도 끝까지 page/CRC를 스캔한다. 압축 payload까지 읽어 원격 입력 비용이 크다.

재현: 첫 20 ms audio page까지 제공하고 두 번째 EOS page를 제한하자 Open이 tail not downloaded로 실패했다. RFC7845 little-endian 헤더/granule과 RFC6716 MSB-first F8 TOC를 사용했다.

수정: 헤더 이후 즉시 시작, 재생 중 CRC/granule/index 수집, 끝에서 duration 확정. DurationKnown=false를 0초 파일과 구분해야 한다.

### 4. P2 — 복제된 Opus parser가 120 ms 상한을 검사하지 않는다

소비자 `internal/audio/opus_toc.go:46`, `internal/player/player.go:63`.

puremux internal/core parser는 상한을 검사하지만 소비자의 복제본은 검사하지 않는다. 반환 samples는 RTP 증가량·pacing·재생 위치에 쓰인다.

재현: FB 07 01 02 03 04 05 06 07. MSB-first TOC config31=20 ms, code3, CBR/no-padding/count7이므로 금지된 140 ms다. 소비자 parser는 6,720 samples로 받아들였다.

수정: pkg/bitstream/opus의 공용 PacketSamples/InspectPacket으로 통일. TOC duration 검사와 전체 packet framing 검증은 계약을 구분해야 한다.

## 정적 코드에서 확인한 통합 위험

### 5. P1 — Seek timeout이 실제 인덱싱을 취소하지 않는다

puremux `internal/format/webm/demux_reader.go:373`, `demux_index.go:34`; 소비자 `internal/player/player.go:534,1314`, `internal/audio/media.go:372`.

로컬 WebM 수정 후에도 첫 명시적 Seek는 completeIndex를 실행한다. Player.Seek는 6초 후 호출자에게 timeout을 반환하지만 seekRequest에 context/cancel이 없고 MediaContext.Seek는 lifetime context를 사용한다. audioLoop의 동기 Seek는 계속 다운로드를 기다릴 수 있다. 그동안 정상 pacing/추가 seek 처리도 지연된다. Stop에 의한 lifetime 취소 경로는 있으므로 영구 교착이라고 단정하지 않는다.

수정: IndexPolicy/SeekPolicy, 현재 확보한 인덱스·가용 범위 안에서만 수행하는 seek, 명시적인 범위 밖 결과. 소비자는 요청 timeout을 실제 demux SeekContext에 연결한다.

### 6. P1 — 라이브의 정상 대기가 fallback으로 바뀐다

puremux `pkg/media/hls.go:455`, `dash.go:265`; 소비자 `internal/player/player.go:919,1163,1184`.

ErrNoNewSegments는 동일 demuxer를 잠시 후 재시도할 수 있는 상태다. producer는 이를 errorCh에 전달하고 종료하며, recovery는 재해석/재오픈하고 CGO에서는 FFmpeg backend를 강제한다. 정상 segment 게시 간격이 fallback·중복 재생·live edge 점프로 바뀔 위험이다. 실제 방송 실행은 하지 않았다.

수정: 취소 가능한 WaitForUpdate/ReadWait 또는 RetryAfter 상태. HLS target duration/DASH update period에 맞춰 기다리고 busy retry를 피한다. URL 재발급/FFmpeg 선택은 소비자 정책으로 남긴다.

### 7. P1 — 가변 Opus에도 gap 계산은 20 ms로 고정된다

소비자 `internal/player/player.go:1571`.

RTP pacing은 TOC samples를 사용하지만 gap 검출은 delta>3000, delta-960, /960이다. 합법적인 120 ms packets가 연속되면 delta=5760이고 실제 손실 없이도 20 ms silence 다섯 개를 넣는다. 코드 산술로 확인했으며 청취 시험은 하지 않았다.

수정: 이전 PTS+실제 Duration과 현재 PTS를 비교한다. 공통 duration/discontinuity 정보는 puremux에 둘 수 있지만 silence 삽입·RTP 송출 시계는 player 책임이다.

### 8. P1/P2 — Opus라는 이유만으로 passthrough하기에는 조건이 부족하다

소비자 `internal/audio/media.go:215,298,411`, `codec_ffmpeg.go:413`, `internal/player/player.go:1599`.

- passthrough는 사실상 IsOpus와 filter 존재 여부만 본다. channel mapping, gain, pre-skip, trimming 판단이 없으며 bestAudio는 채널 수가 많은 쪽을 선호한다.
- Packet이 원본 media.Packet을 내부 소유하므로 trim 정보 자체가 즉시 소멸하는 것은 아니다. 그러나 player/decoder에 제공되는 view에는 DiscardPadding이 없고 decoderPacket은 payload/PTS/DTS/duration만 AVPacket에 복사한다.
- extradata의 OpusHead pre-skip/gain까지 모든 transcoding 경로가 무시한다고 주장하지 않는다. raw passthrough가 컨테이너 의미를 상대 RTP decoder에 전달하지 못하고 packet discard side data도 codec bridge에 넘기지 않는 것이 문제다.

수정: Stream+Packet 기반 passthrough 적합성 검사와 side-data 전달. 불가능하면 소비자의 codec backend로 보낸다. Discord stereo/48 kHz 조건과 sample trimming/decoder는 소비자 책임이다.

### 9. P2 — 요청 seek 위치, decode 시작, audible 시작이 구분되지 않는다

소비자 `internal/audio/media.go:372`, `content_cache.go:484`.

일반 Seek는 선택된 audio track 대신 StreamIndex=-1과 기본 forward 플래그를 사용한다. cache Seek는 80 ms 이상 preroll을 빼고 시작한다. player에는 preroll 출력을 목표 위치까지 버리는 처리가 보이지 않는다. 영상+오디오 cue 선택과 cache/원본 사이 동작 차이 위험이 있다. 실제 청취 결함은 미재현이다.

수정: 대상 audio stream/direction 명시, decode-start와 목표 표시 위치를 구분하는 SeekResult, 소비자의 decode 후 preroll 출력 제거.

### 10. P2 — 늦게 발견한 metadata를 소비자는 여전히 보지 못한다

소비자 `internal/audio/media.go:111,250`.

로컬 puremux의 Info는 재생 중 발견한 Tags를 반영하지만 MediaContext는 생성 시 duration/metadata를 복사한 뒤 조회하지 않는다. 순차 Ogg 등으로 확대하면 unknown duration과 늦은 title이 갱신되지 않는다.

수정: 최신 Info 및 DurationKnown 조회. 새 이벤트 시스템 이전에 기존 API 사용을 바로잡으면 된다.

### 11. P2 — unknown-length/Range 미지원 HTTP 경로가 막혀 있다

소비자 `internal/httpproxy/stream_proxy.go:562`, `internal/audio/media.go:129`; puremux `pkg/media/source_http.go:241`, `pkg/media/open.go:35`.

total이 없는 spool은 Range에 200을 반환하고 OpenHTTP는 ErrNotSeekable을 낸다. 순차 TS도 소비자는 항상 OpenHTTP+자동 probe로 열므로 기존 FormatHint 기반 순차 TS 기능을 사용하지 않는다.

수정: 순차 Source와 HTTP random access capability 분리. bounded disk spool을 Source로 직접 연결하는 옵션도 유용하다. 전체 크기·다운로드된 prefix·유지 중인 seek 범위는 별도 값이어야 한다.

### 12. P2 — 원인 오류와 초기화 비용 계약이 부족하다

puremux `pkg/media/open.go:96,102,108`, `pkg/media/demuxer.go:64`.

하위 오류를 %v로 감싸 errors.Is 원인 체인을 끊는다. MaxProbeBytes는 처음 12-byte 판별만 제한하며 demux 초기화 전체 budget이 아니다.

수정: 원인 wrapping 보존, phase/offset/status/retryability, init bytes/range requests/최대 접근 위치의 선택적 통계. 재시도/URL 갱신/fallback은 외부 정책으로 유지한다.

## 범용 기능 우선순위

| 순서 | puremux 기능 | lavalink-go에서의 가치 |
| --- | --- | --- |
| 1 | 저지연 HTTP Read / 순차·가용 범위 Source | spool 시작·rebuffer 대기 감소 |
| 2 | Playback Open / OnDemand Index / 초기화 budget | WebM·fMP4·Ogg startup 계약 통일 |
| 3 | bounded Seek / 확보된 index와 source 범위 | 다운로드 중 탐색과 timeout 제어 |
| 4 | Live wait/retry 상태와 update 주기 | 정상 live 대기에서 fallback 방지 |
| 5 | 공용 Opus packet inspection | parser 복제 제거, samples·framing 검증 |
| 6 | stream 선택·side data·preroll 계약 | audio-only 처리와 정확한 trim/seek |
| 7 | I/O 관측 및 원인을 보존하는 오류 | metadata/index/download 병목 구분 |

기존 공용 기능도 먼저 활용해야 한다. v0.2.2에 추가한 opus.HeadFromDOPS가 있으므로 소비자의 별도 변환 함수를 유지할 이유가 줄었다. media.Packet.DiscardPadding과 Stream.CodecDelay/SeekPreRoll은 이미 존재한다. 새 필드보다 adapter에서 끝까지 전달하는 일이 우선이다.

권장 순서는 1–4를 먼저 해결하고, 소비자의 gap/trim/passthrough 판단을 병행하는 것이다. Discord 네트워크, FFmpeg decoder, PCM/sample trimming은 puremux에 넣지 않는다.

## 사양 근거

- [RFC6716 §3.2.5](https://www.rfc-editor.org/rfc/rfc6716.html#section-3.2.5): 최대 packet duration 120 ms.
- [RFC7845](https://www.rfc-editor.org/rfc/rfc7845.html): pre-skip, output gain, end trimming.
- [Discord 공식 voice 문서](https://docs.discord.com/developers/topics/voice-connections): 송출 Opus stereo/48 kHz.

이 문서는 발견·설계 제안이다. 실행 재현 네 건과 정적 분석을 구분했으며, 나열한 결함/기능을 수정·구현한 것으로 간주하면 안 된다.
