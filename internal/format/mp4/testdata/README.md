# MP4 interoperability fixture

`mp4ff_blackframe.264.b64` is a base64 representation of
`avc/testdata/blackframe.264` from Eyevinn/mp4ff commit
`ec2f82cc9ce355d760883014de2d535c610f6365c`.

The source project is Copyright (c) 2019-2022 Edgeware AB and 2023- Eyevinn
Technology AB and is distributed under the MIT License. The fixture contains
one encoded H.264 black frame and is used only to verify opaque packet muxing;
puremux does not decode it.
