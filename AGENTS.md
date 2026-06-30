You are an autonomous AI software engineer/architect operating within this repository. You must adhere strictly to the architectural boundaries and operational workflows defined below. Failure to comply will break core project invariants.

## Operational Workflow Constraint (Mandatory)

Before modifying or creating any code, you must read `IMPLEMENTATION_PLAN.md` and `ARCHITECTURE.md`.
**Immediately upon completing any atomic task, modification, or bug fix, you MUST update the Progress Ledger in `IMPLEMENTATION_PLAN.md`.** Increment the status, append logs, change the date, and sign off as your agent instance. Do not skip this step under any circumstances.

## Verification Discipline (Mandatory)

A task is NOT complete when its code is written. A task is complete only when its behavior has been verified. This is non-negotiable.

- **If verification is needed, perform it immediately.** Do not defer it to a later phase, a future turn, or a TODO. Saying "I will verify later" is treated the same as "not done."
- **Synthetic test data you authored yourself is not sufficient verification** for codec/bitstream logic that has a published specification. When a spec exists (AV1 OBU, VP9 superframe, EBML/RFC 8794, WebM), tests MUST be constructed from spec-accurate byte values, and the bit-packing direction (MSB-first vs LSB-first) MUST be confirmed against the spec before the test is considered passing.
- **A test that passes against the wrong byte values is a false pass.** Before marking a codec/detector task Done, re-derive every test byte by hand from the spec and confirm the detector reads the intended field. If you cannot derive it, the task is blocked, not done.
- **Boundary conditions are mandatory test cases.** Truncated packets, nil/empty input, malformed headers, forbidden bits, and size fields that overrun the buffer MUST be covered by tests and MUST NOT panic.
- The Progress Ledger Notes column for any codec/detector or bitstream task MUST record the verification method (e.g. "spec-derived bytes, MSB-first confirmed, boundary cases covered"). A bare "tests pass" note is insufficient.

## Core Architectural Invariants

### 1. Absolute Dependencies Ban

- **NO CGO**: Compiling with `CGO_ENABLED=0` must always succeed. Do not use the `C` import.
- **NO FFmpeg Binary Execution**: Do not import `os/exec` to pipe commands to external `ffmpeg` or `ffprobe` binaries.
- **NO External Decoding**: Under no condition should you write logic that unpacks, decompresses, or inspects individual raw pixel buffers or audio PCM samples. Treat packet payloads as opaque byte slices.

### 2. Design Boundaries

- **Clean Architecture via Interfaces**: All formatting and processing structures must implement decoupled interfaces. Codec-specific variations (e.g., VP9 superframe parsing vs AV1 OBU parsing for keyframe flags) must be isolated behind an interface layer.
- **Strict Separation of Concerns**:
  - `preprocessor` layers must never write bytes to the file/stream.
  - `muxer` layers must never alter timestamps or fix synchronization issues; they assume incoming streams are pristine and ordered.

## Code Quality Standards

- Write idiomatic, clean Go code.
- Minimize memory allocations. High-throughput media routing demands proper memory hygiene. Use `sync.Pool` for packet byte reuse.
- All non-trivial functionality must come with native `_test.go` suites executing completely isolated unit tests using mock writers.
