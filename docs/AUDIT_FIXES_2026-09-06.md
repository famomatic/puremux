# 2026-09-06 감사 수정 결과

작성: Codex `/root`. 기준 감사: [AUDIT_2026-09-06.md](AUDIT_2026-09-06.md).
아래 변경은 작업 트리에 반영됐으며, 새 버전 발행·커밋·push는 하지 않았다.

## 재현된 결함 22개

기존 24개 재현 테스트를 cache overlay에서 저장소의 `_test.go`로 옮겼다.
원래 assertion을 완화하지 않았으며, 현재 모두 통과한다.

| ID | 처리 결과 |
|---|---|
| F01 | rename 전에 Lstat으로 일반 파일 여부를 확인한다. 디렉터리·symlink·특수 파일을 교체하지 않는다. |
| F02 | trun count/flags/필드 길이를 반복문 전에 검증하고 샘플·인덱스·payload 할당을 제한한다. |
| F03 | PTS probe 이후에도 총 AU 수·바이트를 제한한다. 상한에 도달하면 확보한 lookahead로 순서대로 배출한다. picture 수는 증분 관리한다. |
| F04 | TS null PID는 제외하고 허용된 한 번의 duplicate는 중복 처리하지 않는다. PCR 차이는 허용하며 세 번째 duplicate는 거부한다. |
| F05 | PID별 제한된 PSI 조립, pointer, stuffing, CRC, discontinuity를 처리한다. EOF의 미완성 section은 오류다. |
| F06 | PTS reorder의 이전 epoch 복귀와 최초 PTS/DTS의 wrap 경계 정렬을 처리한다. signed timestamp·overflow 검증을 포함한다. |
| F07 | PES 내부 AAC 설정 변경을 거부한다. MP3의 rate/channel 변경도 같은 방식으로 검증한다. |
| F08 | raw ADTS의 profile 변경을 거부한다. |
| F09 | AAC configuration 7을 실제 8채널로 매핑한다. ASC의 encoded index 7은 보존하며 mux 검증은 실제 채널 수를 비교한다. |
| F10 | Ogg page 중간 읽기 실패 시 page 시작으로 복원한다. |
| F11 | WebM element/block 중간 읽기 실패 시 해당 element 시작으로 복원한다. |
| F12 | progressive MP4 seek 실패 시 모든 track cursor를 복원한다. |
| F13 | HLS/DASH candidate segment의 open·prime·seek가 성공한 뒤 기존 segment를 닫는다. 실패하면 이전 재생 상태를 유지한다. |
| F14 | 부족한 MP4 chunk table은 정상 EOF가 아닌 ErrCorrupt다. stsz/stts 개수도 교차 검증한다. |
| F15 | fMP4 순차 재생에서 소비한 fragment metadata를 해제한다. 이후 explicit seek는 필요하면 전체 인덱스를 다시 만든다. |
| F16 | HTTP와 public demuxer의 작업 잠금 대기는 context 취소에 반응한다. |
| F17 | HTTP body 오류도 RetryPolicy에 전달한다. transport/status/body 실패는 하나의 retry budget을 공유하고 매번 identity를 검증한다. |
| F18 | separate-init MP4 오류의 errors.Is 연결을 보존한다. |
| F19 | manifest origin을 rational 값으로 보관하고 목적 time base로 한 번 변환한다. stream/segment별 변환 결과를 캐시한다. |
| F20 | whole-resource segment 뒤에는 implicit range의 과거 offset을 재사용하지 않는다. |
| F21 | 유효한 AV1 level 31을 허용하고 reserved level 24–30은 거부한다. |
| F22 | profile/high_bitdepth와 모순되는 AV1 twelve_bit를 거부한다. |

## 정적 후보 6개

1. **MP4 sample allocation:** 단일 compressed sample은 64 MiB 상한을 적용하며 초과 선언은 할당 전에 거부한다.
2. **다중 sample description:** 복수 stsd entry와 description 전환은 명시적으로 거부한다. 두 개의 유효한 AAC entry로 검증했다.
3. **FLAC false boundary:** header CRC뿐 아니라 frame CRC16, coded number 진행, rate/channel/bit-depth 일관성을 확인한다. [RFC 9639 §9.3 및 D.1.1](https://www.rfc-editor.org/rfc/rfc9639.html#section-9.3)의 실제 예제 frame을 byte-identical하게 읽으며, footer 손상·절단을 거부한다. payload를 디코딩하지 않는다.
4. **PMT 변경:** version만 바뀐 동일 설정은 허용한다. PID/type 추가·제거·변경과 program 전환은 오류로 알린다. CRC가 올바른 PMT 전환 fixture로 확인했다.
5. **출력 I/O 실패 후 재개:** 실제 Write/Seek 실패를 terminal 상태로 보존한다. 이후 WritePacket/Close가 destination을 다시 쓰지 않는다. 입력 검증 오류까지 terminal로 만들지는 않는다.
6. **manifest header 정책:** HLSOptions/DASHOptions.HeaderForURL을 추가했다. 각 manifest·init·segment·key URL에 대한 header를 지정할 수 있다. redirect 정책은 caller의 Client.CheckRedirect가 담당한다.

## 최적화·개선 8개

| 항목 | 반영 내용과 한계 |
|---|---|
| stsc 검색 | reverse linear scan을 binary search로 변경했다. |
| HTTP read-ahead | position query가 cache를 비우지 않는다. MP4의 인접 sample 읽기는 불필요한 seek를 생략한다. 10회 작은 읽기는 probe 포함 2회 HTTP 요청으로 검증했다. 실제 reposition은 기존대로 resource를 재검증한다. |
| seek 비용 | raw audio는 binary search를 사용한다. 단조 audio MP4는 최초 seek에서 1024 sample마다 checkpoint를 만들고 이후 한 block만 검색한다. PTS가 비단조일 수 있는 video에는 일반 exact 비교를 유지한다. |
| 초기화 비용 | OpenOptions.LazyIndex로 ADTS/MP3/FLAC/seekable TS의 header-only 시작을 선택할 수 있다. 재생은 frame/PES 단위이며 explicit seek가 전체 인덱스를 만든다. 기본값은 기존 eager raw/TS 동작이다. |
| packet allocation | MP4/raw audio payload에 4 KiB/64 KiB/1 MiB pool을 적용했다. 더 큰 buffer는 보관하지 않는다. public Packet 객체는 idempotent Release와 오래된 참조의 안전성을 위해 재사용하지 않는다. |
| fMP4 flush | moof 크기를 계산한 뒤 한 번만 encode한다. grouping/offset scratch를 재사용하고 track rotation에 따른 보관량도 제한한다. |
| live byte budget | LiveIngestOptions.MaxBufferBytes를 추가했다. 모든 stream/stage의 retained payload reservation을 release/drop/flush까지 추적한다. |
| HLS 복호화 | fetch가 소유한 ciphertext를 in-place 처리한다. 사용하지 않던 keyCache를 제거했다. 동일 URI의 key rotation을 위해 key는 계속 재조회한다. HTTP caching은 caller의 transport 정책으로 구성할 수 있다. |

성능 배수나 실제 서비스 처리량 향상은 주장하지 않는다. 여기서 검증한 것은
검색/할당 구조, HTTP request count, ownership, 시간 정확성 및 회귀 동작이다.

## 새 옵션과 동작 경계

- `OpenOptions.LazyIndex`: raw audio/seekable TS에 적용한다. duration과 trailing metadata는 EOF까지 미확정일 수 있으므로 최신 Info/Streams를 조회한다. rewind 가능한 Source가 필요하고, explicit seek는 tail을 기다릴 수 있다. 실패한 seek는 기존 재생을 유지한다.
- `OpenMP4WithInitOptions`: separate-init 입력에도 MaxInitBytes와 OnOpen을 제공한다. init bytes와 media Source 읽기를 함께 센다. 기존 OpenMP4WithInit은 기본 옵션으로 호환된다.
- `LiveIngestOptions.MaxBufferBytes`: 기본 64 MiB. 복사 전에 검사하며 초과 시 ErrLiveBufferFull을 반환한다. payload 예산이며 process 전체 RSS나 pool capacity 상한을 의미하지 않는다.
- MP4 reader: trun당 최대 65,536 sample, explicit fragment index 최대 1,048,576 sample, 단일 sample/moov 최대 64 MiB. 큰 정상 입력도 이 자원 제한에 걸릴 수 있다. FLAC frame은 최대 64 MiB다.
- `.m2ts` 자동 output 추론을 제거했다. 이 writer의 출력은 188-byte MPEG-TS이고 CLI는 `.ts`로 안내한다.

## 설계상 미지원 범위

감사의 미지원 목록은 모든 기능을 새로 구현한다는 의미로 해석하지 않았다.
기존 ARCHITECTURE/IMPLEMENTATION_PLAN의 범위를 유지하며 거부 동작과 설명을 명확히 했다.

- HLS는 선택된 variant 또는 audio rendition 하나, DASH는 representation 하나를 읽는다. 외부 A/V 병합·ABR·multi-Period는 제공하지 않는다. multi-Period는 명시적으로 거부한다.
- HLS SAMPLE-AES와 DASH protected representation은 명시적으로 거부한다. DRM 구현과 LL-HLS part 전용 ingestion은 제공하지 않는다.
- Ogg adapter는 선택된 Opus logical stream을 처리한다. 일반 Ogg Vorbis/FLAC 및 chained-stream 전체 재생은 지원 범위가 아니다.
- TS program 변경은 오류이며 multi-program 선택 API와 192-byte M2TS framing은 제공하지 않는다.
- Remux는 NAL/ADTS framing converter가 아니다. fast-start, 일반 metadata 쓰기, subtitle/DRM 등의 기존 제외 범위도 유지한다.
- 순수 sequential WebM/Ogg 입력에는 rewind 가능한 spool이 필요하다.

## 검증

원래 24개 audit regression 외에 published FLAC, sample allocation/description,
PMT 변화, TS partial-read 복구, DASH failed seek, lazy startup/seek 복구,
byte budget, pool ownership, terminal output, queued cancellation,
HTTP request count, separate-init diagnostics 및 sparse seek tests를 추가했다.

MP4/TS/AV1/FLAC fuzz 합계 **905,015회**에서 panic이 없었다.
최종 전체 test/race/386/vet/build/Staticcheck 결과와 실행 조건은
[Progress Ledger](IMPLEMENTATION_PLAN.md)에 기록한다.

gopls MCP는 기존 workspace를 인식하지만 이번에 생성한 파일에 대해
`no package metadata`를 반환했다. 새 파일의 LSP 검증 완료로 표시하지 않았으며,
실제 Go compiler/test/vet와 Staticcheck 검증을 사용했다.


최종 검증 결과 (모든 명령은 MSYS2 UCRT64, cache/temp는 `F:/cache`):

| 검사 | 결과 |
|---|---|
| 전체 `CGO_ENABLED=0 go test -count=1 ./...` | 통과 |
| 전체 `CGO_ENABLED=1 go test -race -count=1 ./...` | 통과, data race 경고 없음 |
| Windows/386 non-CGO 전체 테스트 | 통과 |
| `go vet ./...`, Staticcheck | 통과 |
| non-CGO Windows/amd64, Linux/amd64, Darwin/arm64 build | 통과 |
| `go mod tidy -diff`, `git diff --check`, gofmt | 통과 |
| MP4/TS/AV1/FLAC mutation fuzz | 905,015회, panic 없음 |

실행 로그는 `F:/cache/puremux-audit-20260906/final-*.log`에 있다.
새 source를 포함한 61개 변경 Go 파일에 대해 gopls diagnostics도 요청했지만
MCP의 새 파일 metadata 오류가 재현되어 LSP 검증은 미완료로 남겼다.
이는 위 compiler/test/Staticcheck 검증 결과와 구별한다.
