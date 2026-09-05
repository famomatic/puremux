package media

import "fmt"

func validateOutputMetadata(stream Stream, format Format) error {
	allowed := Disposition(0)
	if format == FormatMPEGTS {
		if stream.Type == MediaAudio {
			allowed = DispositionDefault
		}
		if stream.Language != "" && stream.Language != "und" {
			return fmt.Errorf("%w: MPEG-TS language; set AllowMetadataLoss to discard", ErrUnsupportedFormat)
		}
	}
	if format == FormatWebM || format == FormatMatroska {
		allowed = DispositionDefault
	}
	if stream.Disposition & ^allowed != 0 {
		return fmt.Errorf("%w: %s cannot preserve stream disposition; set AllowMetadataLoss to discard", ErrUnsupportedFormat, format)
	}
	for key, value := range stream.Metadata {
		if value != "" && !((format == FormatWebM || format == FormatMatroska) && key == "title") {
			return fmt.Errorf("%w: %s cannot preserve stream metadata %q; set AllowMetadataLoss to discard", ErrUnsupportedFormat, format, key)
		}
	}
	return nil
}
