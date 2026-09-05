package manifest

import (
	"net/url"
	"testing"
	"time"
)

func TestParseDASHSegmentTemplateTimelineAndInheritance(t *testing.T) {
	base, _ := url.Parse("https://example.test/path/manifest.mpd")
	mpd := `<MPD type="static" mediaPresentationDuration="PT4S">
  <BaseURL>../media/</BaseURL><Period duration="PT4S"><AdaptationSet contentType="audio" mimeType="audio/mp4" codecs="mp4a.40.2" audioSamplingRate="48000">
    <SegmentTemplate timescale="48000" initialization="init-$RepresentationID$.mp4" media="chunk-$Number%05d$-$Time$.m4s" startNumber="7" presentationTimeOffset="48000">
      <SegmentTimeline><S t="48000" d="48000" r="1"/><S t="144000" d="48000" r="-1"/></SegmentTimeline>
    </SegmentTemplate><Representation id="a1" bandwidth="128000"/>
  </AdaptationSet></Period></MPD>`
	parsed, err := ParseDASH(base, []byte(mpd), 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Dynamic || parsed.Duration != 4*time.Second || len(parsed.Representations) != 1 {
		t.Fatalf("unexpected MPD: %+v", parsed)
	}
	rep := parsed.Representations[0]
	if rep.MimeType != "audio/mp4" || rep.Codecs != "mp4a.40.2" || rep.AudioSamplingRate != 48000 || rep.Initialization == nil || rep.Initialization.URI != "https://example.test/media/init-a1.mp4" || len(rep.Segments) != 4 {
		t.Fatalf("unexpected representation: %+v", rep)
	}
	if rep.Segments[0].Number != 7 || rep.Segments[0].Start != 0 || rep.Segments[0].Duration != time.Second || rep.Segments[0].Resource.URI != "https://example.test/media/chunk-00007-48000.m4s" {
		t.Fatalf("unexpected first segment: %+v", rep.Segments[0])
	}
	if rep.Segments[3].Time != 192000 || rep.Segments[3].Start != 3*time.Second {
		t.Fatalf("unexpected repeated segment: %+v", rep.Segments[3])
	}
}

func TestParseDASHSegmentListAndBaseRanges(t *testing.T) {
	base, _ := url.Parse("https://example.test/manifest.mpd")
	mpd := `<MPD mediaPresentationDuration="PT2.5S"><Period><AdaptationSet mimeType="audio/mp4">
 <Representation id="list" bandwidth="10"><BaseURL>file.mp4</BaseURL><SegmentList timescale="1000" duration="1250" startNumber="3"><Initialization range="0-99"/><SegmentURL mediaRange="100-199"/><SegmentURL mediaRange="200-349"/></SegmentList></Representation>
 <Representation id="base" bandwidth="20"><BaseURL>single.mp4</BaseURL><SegmentBase indexRange="100-199"><Initialization range="0-99"/></SegmentBase></Representation>
 </AdaptationSet></Period></MPD>`
	parsed, err := ParseDASH(base, []byte(mpd), 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	list, single := parsed.Representations[0], parsed.Representations[1]
	if list.Initialization == nil || list.Initialization.Range.Offset != 0 || list.Initialization.Range.Length != 100 || len(list.Segments) != 2 || list.Segments[1].Resource.Range.Offset != 200 || list.Segments[1].Resource.Range.Length != 150 || list.Segments[1].Number != 4 {
		t.Fatalf("unexpected SegmentList: %+v", list)
	}
	if !single.SingleFile || single.IndexRange.Offset != 100 || single.IndexRange.Length != 100 || single.Initialization == nil || single.Segments[0].Resource.URI != "https://example.test/single.mp4" {
		t.Fatalf("unexpected SegmentBase: %+v", single)
	}
}

func TestParseDASHBoundaries(t *testing.T) {
	base, _ := url.Parse("https://example.test/manifest.mpd")
	cases := []string{
		``,
		`<NotMPD><Period/></NotMPD>`,
		`<MPD/>`,
		`<MPD><Period/><Period/></MPD>`,
		`<MPD><Period><AdaptationSet><Representation id="x"><SegmentBase indexRange="9-1"/></Representation></AdaptationSet></Period></MPD>`,
		`<MPD><Period><AdaptationSet><SegmentTemplate timescale="1" media="$Unknown$.m4s"><SegmentTimeline><S d="1"/></SegmentTimeline></SegmentTemplate><Representation id="x"/></AdaptationSet></Period></MPD>`,
		`<MPD type="dynamic"><Period><AdaptationSet><SegmentTemplate timescale="1" media="$Time$.m4s"><SegmentTimeline><S d="1" r="-1"/></SegmentTimeline></SegmentTemplate><Representation id="x"/></AdaptationSet></Period></MPD>`,
		`<MPD><Period><AdaptationSet><SegmentTemplate timescale="1" media="$Time$.m4s"><SegmentTimeline><S d="0"/></SegmentTimeline></SegmentTemplate><Representation id="x"/></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT1S"><Period><AdaptationSet><SegmentTemplate timescale="9223372036854775807" presentationTimeOffset="9223372036854775807" media="$Time$.m4s"><SegmentTimeline><S d="1" r="-1"/></SegmentTimeline></SegmentTemplate><Representation id="x"/></AdaptationSet></Period></MPD>`,
		`<MPD><Period><AdaptationSet><Representation id="x"><SegmentList timescale="1" duration="0"><SegmentURL media="a.m4s"/></SegmentList></Representation></AdaptationSet></Period></MPD>`,
		`<MPD mediaPresentationDuration="PT1S"><Period><AdaptationSet><SegmentTemplate timescale="1" duration="1" media="$Number%0999999999d$.m4s"/><Representation id="x"/></AdaptationSet></Period></MPD>`,
	}
	for i, input := range cases {
		if _, err := ParseDASH(base, []byte(input), 2, 2); err == nil {
			t.Fatalf("case %d accepted malformed MPD", i)
		}
	}
}

func TestParseISODuration(t *testing.T) {
	got, err := parseISODuration("P1DT2H3M4.5S")
	if err != nil || got != 26*time.Hour+3*time.Minute+4500*time.Millisecond {
		t.Fatalf("duration=%v err=%v", got, err)
	}
	if _, err := parseISODuration("P1M"); err == nil {
		t.Fatal("calendar-month duration should be unsupported")
	}
	if _, err := parseISODuration("PT"); err == nil {
		t.Fatal("empty duration should be rejected")
	}
}

func TestDASHMinimumUpdatePeriod(t *testing.T) {
	base, _ := url.Parse("https://example.test/live.mpd")
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"PT2.5S", 2500 * time.Millisecond}, {"PT0S", 0}} {
		xml := `<MPD type="dynamic" minimumUpdatePeriod="` + tc.value + `"><Period><AdaptationSet><Representation id="a"><BaseURL>audio.mp4</BaseURL></Representation></AdaptationSet></Period></MPD>`
		m, err := ParseDASH(base, []byte(xml), 10, 10)
		if err != nil || m.MinimumUpdatePeriod != tc.want {
			t.Fatalf("update period %s: %v %v", tc.value, m.MinimumUpdatePeriod, err)
		}
	}
}
