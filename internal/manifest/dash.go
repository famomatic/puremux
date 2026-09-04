package manifest

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type DASHResource struct {
	URI   string
	Range ByteRange
}

type DASHSegment struct {
	Number   uint64
	Time     int64
	Start    time.Duration
	Duration time.Duration
	Resource DASHResource
}

type DASHRepresentation struct {
	ID                string
	MimeType          string
	ContentType       string
	Codecs            string
	Bandwidth         int64
	AudioSamplingRate int
	BaseURL           string
	Initialization    *DASHResource
	IndexRange        ByteRange
	Segments          []DASHSegment
	SingleFile        bool
}

type DASHManifest struct {
	Dynamic         bool
	Duration        time.Duration
	Representations []DASHRepresentation
}

type mpdXML struct {
	XMLName                   xml.Name        `xml:"MPD"`
	Type                      string          `xml:"type,attr"`
	MediaPresentationDuration string          `xml:"mediaPresentationDuration,attr"`
	BaseURL                   string          `xml:"BaseURL"`
	Periods                   []dashPeriodXML `xml:"Period"`
}

type dashPeriodXML struct {
	Duration       string                 `xml:"duration,attr"`
	BaseURL        string                 `xml:"BaseURL"`
	AdaptationSets []dashAdaptationSetXML `xml:"AdaptationSet"`
}

type dashAdaptationSetXML struct {
	MimeType          string                  `xml:"mimeType,attr"`
	ContentType       string                  `xml:"contentType,attr"`
	Codecs            string                  `xml:"codecs,attr"`
	AudioSamplingRate int                     `xml:"audioSamplingRate,attr"`
	BaseURL           string                  `xml:"BaseURL"`
	SegmentBase       *dashSegmentBaseXML     `xml:"SegmentBase"`
	SegmentList       *dashSegmentListXML     `xml:"SegmentList"`
	SegmentTemplate   *dashSegmentTemplateXML `xml:"SegmentTemplate"`
	Representations   []dashRepresentationXML `xml:"Representation"`
}

type dashRepresentationXML struct {
	ID                string                  `xml:"id,attr"`
	MimeType          string                  `xml:"mimeType,attr"`
	ContentType       string                  `xml:"contentType,attr"`
	Codecs            string                  `xml:"codecs,attr"`
	Bandwidth         int64                   `xml:"bandwidth,attr"`
	AudioSamplingRate int                     `xml:"audioSamplingRate,attr"`
	BaseURL           string                  `xml:"BaseURL"`
	SegmentBase       *dashSegmentBaseXML     `xml:"SegmentBase"`
	SegmentList       *dashSegmentListXML     `xml:"SegmentList"`
	SegmentTemplate   *dashSegmentTemplateXML `xml:"SegmentTemplate"`
}

type dashInitializationXML struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

type dashSegmentBaseXML struct {
	IndexRange     string                 `xml:"indexRange,attr"`
	Initialization *dashInitializationXML `xml:"Initialization"`
}

type dashSegmentURLXML struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

type dashSegmentListXML struct {
	Timescale              int64                  `xml:"timescale,attr"`
	Duration               int64                  `xml:"duration,attr"`
	StartNumber            uint64                 `xml:"startNumber,attr"`
	PresentationTimeOffset int64                  `xml:"presentationTimeOffset,attr"`
	Initialization         *dashInitializationXML `xml:"Initialization"`
	SegmentURLs            []dashSegmentURLXML    `xml:"SegmentURL"`
}

type dashSegmentTemplateXML struct {
	Timescale              int64                   `xml:"timescale,attr"`
	Duration               int64                   `xml:"duration,attr"`
	StartNumber            uint64                  `xml:"startNumber,attr"`
	PresentationTimeOffset int64                   `xml:"presentationTimeOffset,attr"`
	Media                  string                  `xml:"media,attr"`
	Initialization         string                  `xml:"initialization,attr"`
	Timeline               *dashSegmentTimelineXML `xml:"SegmentTimeline"`
}

type dashSegmentTimelineXML struct {
	Entries []dashSElementXML `xml:"S"`
}

type dashSElementXML struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr"`
}

func ParseDASH(base *url.URL, data []byte, maxRepresentations, maxSegments int) (DASHManifest, error) {
	if base == nil {
		return DASHManifest{}, errors.New("dash: nil base URL")
	}
	if maxRepresentations <= 0 {
		maxRepresentations = 1000
	}
	if maxSegments <= 0 {
		maxSegments = 100000
	}
	var document mpdXML
	if err := xml.Unmarshal(data, &document); err != nil {
		return DASHManifest{}, fmt.Errorf("dash: invalid MPD XML: %w", err)
	}
	if len(document.Periods) == 0 {
		return DASHManifest{}, errors.New("dash: MPD has no Period")
	}
	if len(document.Periods) > 1 {
		return DASHManifest{}, errors.New("dash: multi-Period MPDs are not supported")
	}
	duration, err := parseISODuration(document.MediaPresentationDuration)
	if err != nil {
		return DASHManifest{}, err
	}
	documentBase, err := resolveDASH(base, document.BaseURL)
	if err != nil {
		return DASHManifest{}, err
	}
	manifest := DASHManifest{Dynamic: strings.EqualFold(document.Type, "dynamic"), Duration: duration}
	for _, period := range document.Periods {
		periodDuration := duration
		if period.Duration != "" {
			periodDuration, err = parseISODuration(period.Duration)
			if err != nil {
				return DASHManifest{}, err
			}
		}
		periodBase, err := resolveDASH(documentBase, period.BaseURL)
		if err != nil {
			return DASHManifest{}, err
		}
		for _, adaptation := range period.AdaptationSets {
			adaptationBase, err := resolveDASH(periodBase, adaptation.BaseURL)
			if err != nil {
				return DASHManifest{}, err
			}
			for _, raw := range adaptation.Representations {
				if len(manifest.Representations) >= maxRepresentations {
					return DASHManifest{}, errors.New("dash: too many representations")
				}
				repBase, err := resolveDASH(adaptationBase, raw.BaseURL)
				if err != nil {
					return DASHManifest{}, err
				}
				rep := DASHRepresentation{ID: raw.ID, Bandwidth: raw.Bandwidth, MimeType: firstNonEmpty(raw.MimeType, adaptation.MimeType), ContentType: firstNonEmpty(raw.ContentType, adaptation.ContentType), Codecs: firstNonEmpty(raw.Codecs, adaptation.Codecs), AudioSamplingRate: firstNonZero(raw.AudioSamplingRate, adaptation.AudioSamplingRate), BaseURL: repBase.String()}
				segmentBase := mergeSegmentBase(adaptation.SegmentBase, raw.SegmentBase)
				segmentList := mergeSegmentList(adaptation.SegmentList, raw.SegmentList)
				segmentTemplate := mergeSegmentTemplate(adaptation.SegmentTemplate, raw.SegmentTemplate)
				switch {
				case segmentTemplate != nil:
					err = buildTemplateSegments(&rep, repBase, segmentTemplate, periodDuration, maxSegments)
				case segmentList != nil:
					err = buildListSegments(&rep, repBase, segmentList, maxSegments)
				case segmentBase != nil:
					err = buildBaseSegment(&rep, repBase, segmentBase)
				default:
					rep.SingleFile = true
					rep.Segments = []DASHSegment{{Number: 1, Duration: periodDuration, Resource: DASHResource{URI: repBase.String()}}}
				}
				if err != nil {
					return DASHManifest{}, fmt.Errorf("dash: representation %q: %w", rep.ID, err)
				}
				manifest.Representations = append(manifest.Representations, rep)
			}
		}
	}
	if len(manifest.Representations) == 0 {
		return DASHManifest{}, errors.New("dash: MPD has no Representation")
	}
	return manifest, nil
}

func buildBaseSegment(rep *DASHRepresentation, base *url.URL, segmentBase *dashSegmentBaseXML) error {
	rep.SingleFile = true
	if segmentBase.IndexRange != "" {
		parsed, err := parseDASHRange(segmentBase.IndexRange)
		if err != nil {
			return err
		}
		rep.IndexRange = parsed
	}
	if segmentBase.Initialization != nil {
		resource, err := dashResource(base, segmentBase.Initialization.SourceURL, segmentBase.Initialization.Range)
		if err != nil {
			return err
		}
		rep.Initialization = &resource
	}
	rep.Segments = []DASHSegment{{Number: 1, Resource: DASHResource{URI: base.String()}}}
	return nil
}

func buildListSegments(rep *DASHRepresentation, base *url.URL, list *dashSegmentListXML, maxSegments int) error {
	timescale := list.Timescale
	if timescale == 0 {
		timescale = 1
	}
	if timescale < 0 || list.Duration <= 0 || len(list.SegmentURLs) > maxSegments {
		return errors.New("invalid SegmentList bounds")
	}
	if list.Initialization != nil {
		resource, err := dashResource(base, list.Initialization.SourceURL, list.Initialization.Range)
		if err != nil {
			return err
		}
		rep.Initialization = &resource
	}
	startNumber := list.StartNumber
	if startNumber == 0 {
		startNumber = 1
	}
	for i, raw := range list.SegmentURLs {
		resource, err := dashResource(base, raw.Media, raw.MediaRange)
		if err != nil {
			return err
		}
		timestamp, ok := safeMulInt64(int64(i), list.Duration)
		if !ok {
			return errors.New("SegmentList timestamp overflow")
		}
		startTicks, ok := safeSubInt64(timestamp, list.PresentationTimeOffset)
		if !ok || uint64(i) > ^uint64(0)-startNumber {
			return errors.New("SegmentList timestamp or number overflow")
		}
		start, err := scaleDASH(startTicks, timescale)
		if err != nil {
			return err
		}
		duration, err := scaleDASH(list.Duration, timescale)
		if err != nil {
			return err
		}
		rep.Segments = append(rep.Segments, DASHSegment{Number: startNumber + uint64(i), Time: timestamp, Start: start, Duration: duration, Resource: resource})
	}
	if len(rep.Segments) == 0 {
		return errors.New("empty SegmentList")
	}
	return nil
}

func buildTemplateSegments(rep *DASHRepresentation, base *url.URL, template *dashSegmentTemplateXML, periodDuration time.Duration, maxSegments int) error {
	timescale := template.Timescale
	if timescale == 0 {
		timescale = 1
	}
	if timescale < 0 || template.Media == "" {
		return errors.New("invalid SegmentTemplate")
	}
	startNumber := template.StartNumber
	if startNumber == 0 {
		startNumber = 1
	}
	if template.Initialization != "" {
		name, err := expandDASHTemplate(template.Initialization, rep, startNumber, 0)
		if err != nil {
			return err
		}
		resource, err := dashResource(base, name, "")
		if err != nil {
			return err
		}
		rep.Initialization = &resource
	}
	var times []int64
	var durations []int64
	if template.Timeline != nil {
		current := int64(0)
		for i, entry := range template.Timeline.Entries {
			if entry.D <= 0 || entry.R < -1 {
				return errors.New("invalid SegmentTimeline entry")
			}
			if entry.T != nil {
				current = *entry.T
			}
			repeat := entry.R
			if repeat == -1 {
				var end int64
				if i+1 < len(template.Timeline.Entries) && template.Timeline.Entries[i+1].T != nil {
					end = *template.Timeline.Entries[i+1].T
				} else if periodDuration > 0 {
					periodTicks, tickErr := durationToTicks(periodDuration, timescale)
					if tickErr != nil {
						return tickErr
					}
					var ok bool
					end, ok = safeAddInt64(periodTicks, template.PresentationTimeOffset)
					if !ok {
						return errors.New("SegmentTimeline bound overflow")
					}
				} else {
					return errors.New("open-ended SegmentTimeline has no bound")
				}
				if end <= current {
					return errors.New("invalid open-ended SegmentTimeline")
				}
				distance := new(big.Int).Sub(big.NewInt(end), big.NewInt(current))
				distance.Add(distance, big.NewInt(entry.D-1))
				distance.Quo(distance, big.NewInt(entry.D))
				distance.Sub(distance, big.NewInt(1))
				if !distance.IsInt64() {
					return errors.New("SegmentTimeline repeat overflow")
				}
				repeat = distance.Int64()
			}
			if repeat > int64(maxSegments-len(times)-1) {
				return errors.New("too many SegmentTimeline entries")
			}
			for count := int64(0); count <= repeat; count++ {
				times = append(times, current)
				durations = append(durations, entry.D)
				var ok bool
				current, ok = safeAddInt64(current, entry.D)
				if !ok {
					return errors.New("SegmentTimeline timestamp overflow")
				}
			}
		}
	} else {
		if template.Duration <= 0 || periodDuration <= 0 {
			return errors.New("SegmentTemplate without timeline needs duration and Period duration")
		}
		periodTicks, err := durationToTicks(periodDuration, timescale)
		if err != nil {
			return err
		}
		countBig := new(big.Int).Add(big.NewInt(periodTicks), big.NewInt(template.Duration-1))
		countBig.Quo(countBig, big.NewInt(template.Duration))
		if !countBig.IsInt64() {
			return errors.New("template segment count overflow")
		}
		count := countBig.Int64()
		if count > int64(maxSegments) {
			return errors.New("too many template segments")
		}
		for i := int64(0); i < count; i++ {
			timestamp, ok := safeMulInt64(i, template.Duration)
			if !ok {
				return errors.New("template timestamp overflow")
			}
			times = append(times, timestamp)
			durations = append(durations, template.Duration)
		}
	}
	for i, timestamp := range times {
		if uint64(i) > ^uint64(0)-startNumber {
			return errors.New("template segment number overflow")
		}
		number := startNumber + uint64(i)
		name, err := expandDASHTemplate(template.Media, rep, number, timestamp)
		if err != nil {
			return err
		}
		resource, err := dashResource(base, name, "")
		if err != nil {
			return err
		}
		startTicks, ok := safeSubInt64(timestamp, template.PresentationTimeOffset)
		if !ok {
			return errors.New("template presentation timestamp overflow")
		}
		start, err := scaleDASH(startTicks, timescale)
		if err != nil {
			return err
		}
		duration, err := scaleDASH(durations[i], timescale)
		if err != nil {
			return err
		}
		rep.Segments = append(rep.Segments, DASHSegment{Number: number, Time: timestamp, Start: start, Duration: duration, Resource: resource})
	}
	if len(rep.Segments) == 0 {
		return errors.New("empty SegmentTemplate")
	}
	return nil
}

func mergeSegmentList(parent, child *dashSegmentListXML) *dashSegmentListXML {
	if child == nil {
		return parent
	}
	out := *child
	if parent != nil {
		if out.Timescale == 0 {
			out.Timescale = parent.Timescale
		}
		if out.Duration == 0 {
			out.Duration = parent.Duration
		}
		if out.StartNumber == 0 {
			out.StartNumber = parent.StartNumber
		}
		if out.PresentationTimeOffset == 0 {
			out.PresentationTimeOffset = parent.PresentationTimeOffset
		}
		if out.Initialization == nil {
			out.Initialization = parent.Initialization
		}
		if len(out.SegmentURLs) == 0 {
			out.SegmentURLs = parent.SegmentURLs
		}
	}
	return &out
}

func mergeSegmentBase(parent, child *dashSegmentBaseXML) *dashSegmentBaseXML {
	if child == nil {
		return parent
	}
	out := *child
	if parent != nil {
		if out.IndexRange == "" {
			out.IndexRange = parent.IndexRange
		}
		if out.Initialization == nil {
			out.Initialization = parent.Initialization
		}
	}
	return &out
}

func mergeSegmentTemplate(parent, child *dashSegmentTemplateXML) *dashSegmentTemplateXML {
	if child == nil {
		return parent
	}
	out := *child
	if parent != nil {
		if out.Timescale == 0 {
			out.Timescale = parent.Timescale
		}
		if out.Duration == 0 {
			out.Duration = parent.Duration
		}
		if out.StartNumber == 0 {
			out.StartNumber = parent.StartNumber
		}
		if out.PresentationTimeOffset == 0 {
			out.PresentationTimeOffset = parent.PresentationTimeOffset
		}
		if out.Media == "" {
			out.Media = parent.Media
		}
		if out.Initialization == "" {
			out.Initialization = parent.Initialization
		}
		if out.Timeline == nil {
			out.Timeline = parent.Timeline
		}
	}
	return &out
}

var templateToken = regexp.MustCompile(`\$(RepresentationID|Bandwidth|Number|Time)(%0([0-9]+)d)?\$`)

func expandDASHTemplate(value string, rep *DASHRepresentation, number uint64, timestamp int64) (string, error) {
	sentinel := "\x00DOLLAR\x00"
	value = strings.ReplaceAll(value, "$$", sentinel)
	value = templateToken.ReplaceAllStringFunc(value, func(token string) string {
		match := templateToken.FindStringSubmatch(token)
		var raw string
		switch match[1] {
		case "RepresentationID":
			raw = rep.ID
		case "Bandwidth":
			raw = strconv.FormatInt(rep.Bandwidth, 10)
		case "Number":
			raw = strconv.FormatUint(number, 10)
		case "Time":
			raw = strconv.FormatInt(timestamp, 10)
		}
		if match[3] != "" && match[1] != "RepresentationID" {
			width, err := strconv.Atoi(match[3])
			if err != nil || width > 64 {
				return "\x00INVALID_WIDTH\x00"
			}
			if len(raw) < width {
				raw = strings.Repeat("0", width-len(raw)) + raw
			}
		}
		return raw
	})
	value = strings.ReplaceAll(value, sentinel, "$")
	if strings.Contains(value, "\x00INVALID_WIDTH\x00") {
		return "", errors.New("template format width exceeds 64 digits")
	}
	if strings.Contains(value, "$") {
		return "", errors.New("unsupported or malformed template token")
	}
	return value, nil
}

func dashResource(base *url.URL, reference, byteRange string) (DASHResource, error) {
	resolved, err := resolveDASH(base, reference)
	if err != nil {
		return DASHResource{}, err
	}
	resource := DASHResource{URI: resolved.String()}
	if byteRange != "" {
		resource.Range, err = parseDASHRange(byteRange)
	}
	return resource, err
}

func parseDASHRange(value string) (ByteRange, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return ByteRange{}, errors.New("invalid byte range")
	}
	start, err1 := strconv.ParseInt(parts[0], 10, 64)
	end, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start || end == math.MaxInt64 {
		return ByteRange{}, errors.New("invalid byte range")
	}
	return ByteRange{Offset: start, Length: end - start + 1, Valid: true}, nil
}

func resolveDASH(base *url.URL, reference string) (*url.URL, error) {
	if reference == "" {
		copy := *base
		return &copy, nil
	}
	ref, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return nil, err
	}
	resolved := base.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return nil, errors.New("dash: only HTTP(S) resources are supported")
	}
	return resolved, nil
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

func parseISODuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	match := isoDuration.FindStringSubmatch(value)
	if match == nil {
		return 0, errors.New("dash: unsupported ISO 8601 duration")
	}
	if match[1] == "" && match[2] == "" && match[3] == "" && match[4] == "" {
		return 0, errors.New("dash: empty ISO 8601 duration")
	}
	total := float64(0)
	for i, scale := range []float64{86400, 3600, 60, 1} {
		if match[i+1] != "" {
			part, err := strconv.ParseFloat(match[i+1], 64)
			if err != nil {
				return 0, err
			}
			total += part * scale
		}
	}
	if total > float64(math.MaxInt64)/float64(time.Second) {
		return 0, errors.New("dash: duration overflow")
	}
	return time.Duration(total * float64(time.Second)), nil
}

func scaleDASH(value, timescale int64) (time.Duration, error) {
	if timescale <= 0 {
		return 0, errors.New("dash: invalid timescale")
	}
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(int64(time.Second)))
	n.Quo(n, big.NewInt(timescale))
	if !n.IsInt64() {
		return 0, errors.New("dash: scaled time overflow")
	}
	return time.Duration(n.Int64()), nil
}

func durationToTicks(value time.Duration, timescale int64) (int64, error) {
	if timescale <= 0 {
		return 0, errors.New("dash: invalid timescale")
	}
	n := new(big.Int).Mul(big.NewInt(int64(value)), big.NewInt(timescale))
	n.Quo(n, big.NewInt(int64(time.Second)))
	if !n.IsInt64() {
		return 0, errors.New("dash: duration tick overflow")
	}
	return n.Int64(), nil
}

func safeAddInt64(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

func safeSubInt64(a, b int64) (int64, bool) {
	n := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

func safeMulInt64(a, b int64) (int64, bool) {
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
