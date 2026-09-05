# 2026-09-05 감사 수정 결과

작성: Codex `/root`. 원본: [감사 기록](AUDIT_2026-09-05.md). 작업 내역: [Progress Ledger](IMPLEMENTATION_PLAN.md), 작업 43–57.

## 재현 결함

| 감사 번호 | 수정 결과 |
| --- | --- |
| 1 | HLS/DASH current/waiting/EOF 전이를 정리하여 새 세그먼트 부재·네트워크 실패 후 재시도와 반복 EOF를 보장. 새 세그먼트 없이 종료 manifest로 바뀌는 경우도 종료 인식. |
| 2 | wrapper root 취소를 실제 ReadPacket/Seek 작업 context로 전달하여 DASH Close 교착 제거. |
| 3 | jitter 버퍼 포화 시 가장 오래된 패킷을 방출하여 정상 입력의 지속적인 폐기·출력 정지 방지. |
| 4 | Ogg Opus EOS 이전 page의 clock을 유지하고 끝부분 제거량을 Packet.DiscardPadding으로 전달. |
| 5 | MP4 edit-list shift를 DTS와 PTS에 함께 적용. |
| 6 | stss 부재와 empty stss를 구분하여 잘못된 keyframe 판정·seek 방지. |
| 7 | TS 영상의 없는 duration은 unknown으로 표시하고, wire에 duration이 없는 TS 출력은 이를 허용. Known zero/negative duration은 여전히 거부. |
| 8 | dOps→OpusHead, dfLa→Matroska FLAC private 역변환 추가. |
| 9 | manifest seek에 실제 segment shift를 적용하고 checked arithmetic 사용. |
| 10 | MP4 native-tick seek로 변환 왕복에 따른 1 tick 손실 제거. |
| 11 | redirect 최종 URL을 manifest 상대 경로와 refresh의 기준으로 사용. |
| 12 | edit-list 변환의 중간 곱셈 overflow와 DTS/PTS 덧셈 overflow 방어. |
| 13 | MP4 top-level box/source 경계를 확인하고 mdat payload를 seek로 건너뜀. |

## 정적 검토 및 성능 개선

- HLS 초기화 캐시는 현재 EXT-X-MAP 한 항목만 유지하며 각 fetch는 MaxSegmentBytes에 제한된다.
- TS streaming 대기열은 4,096 packets/64 MiB, PES 하나는 64 MiB 상한이다. Seekable TS는 같은 streaming parser를 이용해 원본 전체·완성 PES의 중복 복사본을 없앴으며 보유 payload 256 MiB/1,000,000 packets 상한을 둔다.
- Ogg 여러 page에 이어진 packet은 16 MiB로 제한한다. adapter의 중복 packet 복사를 제거했다.
- TS keyframe 판정은 core Detector interface를 재사용한다. HEVC forbidden bit/temporal_id_plus1/truncated header를 검증한다.
- MP4는 ISO-639-2 소문자 3자 언어를 보존한다. WebM/Matroska는 언어·트랙 제목·기본 트랙 표시를 보존한다.
- writer가 지원하지 않는 메타데이터는 명시적으로 거부한다. `MuxOptions.AllowMetadataLoss=true`일 때만 생략을 허용한다. Remux의 container tag도 이 정책에 포함한다. `muxing_app`/`writing_app`은 출력 writer의 provenance로 대체한다.
- MP4와 TS가 표현하지 못하는 `DiscardPadding`은 `ErrUnsupportedFormat`을 반환한다. 이 값은 `AllowMetadataLoss`로 무시할 수 없다. 압축 payload를 바꾸거나 duration을 임의로 줄이지 않는다.
- 파일 설치 실패 후 원본 복구까지 실패하면 두 오류와 보존된 backup 경로를 함께 반환한다. 임시 디렉터리에서 rename fault injection으로 원본 보존을 확인했다.
- fMP4 duration 범위를 증분 유지하고 payload 복사 두 단계를 제거했다. fragment metadata는 65,536 packets로 제한하며 1 MiB를 넘는 buffer는 전역 pool에 되돌리지 않는다.
- MP4 fragment 정렬은 전체 parse 종료 후 한 번 수행한다. Progressive seek는 한 번의 스캔에서 후보·fallback과 cursor를 보존하여 반복 스캔을 제거했다.
- HTTP 순차 읽기에 32 KiB read-ahead를 추가했다. ReaderAt은 독립적인 Range 요청을 유지하고 Seek 후에는 validator를 다시 검사한다. 1,000번의 1-byte Read가 probe와 data 요청 두 번으로 처리된다.
- FragmentDuration은 audio-only fragment에 적용한다. 영상은 GOP 기준이며 byte/packet 상한이 강제하는 cut은 non-keyframe에서 발생할 수 있다.

## 검증

MSYS2 UCRT64, 캐시 `F:/cache`에서 다음 검사가 모두 통과했다.

```sh
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test ./pkg/bitstream/opus -run '^$' -fuzz '^FuzzOpusConfigurations$' -fuzztime=10s -parallel=2
git diff --check
```

Fuzz는 1,274,505회 실행했으며 실패가 없었다. Race 검사용 CGO 활성화는 제품 의존성 추가가 아니다. 제품의 CGO 비활성 빌드가 통과한다.

기존 감사 15개 top-level 테스트는 저장소의 `audit_regression_test.go`에 편입했다. 추가 회귀 검사는 buffer 상한, codec forbidden/truncated bytes, HTTP 요청 수/변경 검출, metadata roundtrip, 복구 실패를 다룬다. AAC ASC의 MSB-first 값, MP4 empty stss 및 language bit packing, Ogg/Opus little-endian granule·EOS semantics와 기존 독립 MP4 parser 검증을 사용했다. 모든 입력·플랫폼에 대한 완전성 증명은 아니다.

## 그대로 남는 명시적 지원 범위

감사에서 결함과 별도로 분류했던 기능은 이 변경에서 새로 구현하지 않았다: HLS/DASH 별도 audio/video rendition 병합, DASH multi-Period 및 일부 dynamic timeline, elementary framing 변환, LiveMuxer stream별 종료/idle watermark, Ogg의 Opus 외 코덱, MPEG-TS 외 non-seekable Open, CENC/DRM, fast-start relocation, manifest 생성 등이다.

메타데이터·discard padding도 위 지원표 밖의 wire 표현을 새로 구현한 것은 아니며, 무음 손실을 명시적 오류로 바꾼 것이다. 호출자는 이 오류를 처리해야 한다.

성능 한계도 남는다. Progressive MP4 seek는 여전히 sample 수에 비례하는 한 번의 스캔이며 run-prefix/binary-search index는 추가하지 않았다. Seekable TS는 bounded in-memory packet index를 사용하며 source-offset lazy payload reader는 아니다. 이 때문에 큰 TS 녹화는 지정 상한에서 오류가 난다. StreamingInputReader를 사용하는 non-seekable 경로는 전체 녹화를 보유하지 않는다. 각 자원 상한은 해당 자료구조의 상한이며 프로세스 전체 메모리 예산을 보증하지 않는다.
