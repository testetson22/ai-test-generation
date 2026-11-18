package report

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseJUnitReportPreservesTestIdentityAndOutcomes(t *testing.T) {
	data := []byte(`<?xml version="1.0"?>
<testsuites><testsuite name="unit">
  <testcase classname="pkg.PassingTest" name="passes"/>
  <testcase classname="pkg.FailingTest" name="fails"><failure message="assertion failed">details</failure></testcase>
  <testcase classname="pkg.SkippedTest" name="skips"><skipped/></testcase>
  <testcase classname="pkg.OtherTest" name="unexpected"/>
</testsuite></testsuites>`)

	summary, err := Parse(JUnitMediaType, data, []string{
		"pkg.PassingTest#passes", "pkg.FailingTest#fails", "pkg.SkippedTest#skips", "pkg.MissingTest#missing",
	}, 4096)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if summary.MediaType != JUnitMediaType || summary.EvidenceComplete {
		t.Fatalf("media type or completeness = %#v", summary)
	}
	assertIDs(t, summary.ReportedTestIDs, "pkg.FailingTest#fails", "pkg.OtherTest#unexpected", "pkg.PassingTest#passes", "pkg.SkippedTest#skips")
	assertIDs(t, summary.ExecutedTestIDs, "pkg.FailingTest#fails", "pkg.OtherTest#unexpected", "pkg.PassingTest#passes")
	assertIDs(t, summary.SkippedTestIDs, "pkg.SkippedTest#skips")
	assertIDs(t, summary.FailedTestIDs, "pkg.FailingTest#fails")
	assertIDs(t, summary.MissingTestIDs, "pkg.MissingTest#missing")
	assertIDs(t, summary.UnexpectedTestIDs, "pkg.OtherTest#unexpected")
}

func TestParseJUnitReportCompleteEvidenceIsNotVerdict(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	summary, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs"}, 1024)
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if !summary.EvidenceComplete || len(summary.FailedTestIDs) != 0 || len(summary.ExecutedTestIDs) != 1 {
		t.Fatalf("report summary = %#v", summary)
	}
}

func TestParseRejectsUnsupportedTestcaseOutcomeAttributes(t *testing.T) {
	for _, attributes := range []string{
		`status="notrun" result="suppressed"`, `status="notrun"`, `result="suppressed"`,
		`status="run" result="completed"`, `status=""`, `result="unknown"`,
		`disabled="true"`, `skipped="true"`, `executed="false"`,
		`xmlns:x="urn:unknown" x:status="notrun"`,
	} {
		t.Run(attributes, func(t *testing.T) {
			data := []byte(`<testsuite><testcase classname="pkg.Test" name="already-read"/><testcase classname="pkg.Test" name="runs" ` + attributes + `/></testsuite>`)
			summary, err := Parse(JUnitMediaType, data, []string{"pkg.Test#already-read", "pkg.Test#runs"}, 4096)
			if !errors.Is(err, ErrInvalidReport) || summary.EvidenceComplete || len(summary.ExecutedTestIDs) != 0 {
				t.Fatalf("unsupported testcase outcome returned summary=%#v, error=%v", summary, err)
			}
		})
	}
}

func TestParseRejectsSuitesEmbeddedInReportData(t *testing.T) {
	for _, container := range []string{"system-out", "system-err", "properties", "property", "unknown"} {
		for _, suite := range []string{"testsuite", "testsuites"} {
			t.Run(container+"/"+suite, func(t *testing.T) {
				data := []byte(`<testsuite><` + container + `><` + suite + `><testsuite><testcase classname="pkg.Test" name="runs"/></testsuite></` + suite + `></` + container + `></testsuite>`)
				summary, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs"}, 4096)
				if !errors.Is(err, ErrInvalidReport) || summary.EvidenceComplete || len(summary.ExecutedTestIDs) != 0 {
					t.Fatalf("embedded suite returned summary=%#v, error=%v", summary, err)
				}
			})
		}
	}
}

func TestParseAcceptsNestedSuitesAndTreatsOutputAsText(t *testing.T) {
	data := []byte(`<testsuites><testsuite><properties><property name="fixture" value="unit"/></properties>
<system-out><![CDATA[<testsuite><testcase classname="fake.Test" name="not-executed"/></testsuite>]]></system-out>
<testsuite><testcase classname="pkg.Test" name="runs" time="0.01" assertions="1" file="test.py" line="1"/>
<testcase classname="pkg.Test" name="skips"><skipped/></testcase></testsuite></testsuite></testsuites>`)
	summary, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs", "pkg.Test#skips"}, 4096)
	if err != nil {
		t.Fatalf("parse nested suites: %v", err)
	}
	assertIDs(t, summary.ReportedTestIDs, "pkg.Test#runs", "pkg.Test#skips")
	assertIDs(t, summary.ExecutedTestIDs, "pkg.Test#runs")
	assertIDs(t, summary.SkippedTestIDs, "pkg.Test#skips")
	if !summary.EvidenceComplete || len(summary.FailedTestIDs) != 0 || len(summary.UnexpectedTestIDs) != 0 {
		t.Fatalf("nested suite summary = %#v", summary)
	}
}

func TestParseRejectsUnsupportedAndUnboundedInput(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	if _, err := Parse("application/json", data, []string{"pkg.Test#runs"}, 1024); !errors.Is(err, ErrUnsupportedMediaType) {
		t.Fatalf("unsupported media type error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs"}, int64(len(data)-1)); !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, data, nil, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty expected inventory error = %v", err)
	}
	if _, err := Parse(JUnitMediaType, nil, []string{"pkg.Test#runs"}, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty report error = %v", err)
	}
}

func TestParseRejectsUnsafeOrAmbiguousXML(t *testing.T) {
	validCase := `<testcase classname="pkg.Test" name="runs"/>`
	tests := []struct {
		name string
		xml  string
	}{
		{name: "doctype", xml: `<!DOCTYPE testsuite [<!ENTITY x "y">]><testsuite>` + validCase + `</testsuite>`},
		{name: "duplicate identity", xml: `<testsuite>` + validCase + validCase + `</testsuite>`},
		{name: "conflicting outcomes", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><failure/><skipped/></testcase></testsuite>`},
		{name: "nested testcase", xml: `<testsuite><testcase classname="pkg.Test" name="runs">` + validCase + `</testcase></testsuite>`},
		{name: "nested failure", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><system-out><failure/></system-out></testcase></testsuite>`},
		{name: "unknown testcase child", xml: `<testsuite><testcase classname="pkg.Test" name="runs"><result status="passed"/></testcase></testsuite>`},
		{name: "malformed document", xml: `<testsuite>` + validCase},
		{name: "no test cases", xml: `<testsuite/>`},
		{name: "processing instruction", xml: `<?unexpected data?><testsuite>` + validCase + `</testsuite>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(JUnitMediaType, []byte(test.xml), []string{"pkg.Test#runs"}, 4096); !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestParseRejectsExcessiveXMLDepth(t *testing.T) {
	data := `<testsuite>` + strings.Repeat("<x>", MaxXMLDepth) + strings.Repeat("</x>", MaxXMLDepth) + `</testsuite>`
	if _, err := Parse(JUnitMediaType, []byte(data), []string{"pkg.Test#runs"}, 4096); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("depth error = %v", err)
	}
}

func TestParseRejectsDuplicateExpectedIdentities(t *testing.T) {
	data := []byte(`<testsuite><testcase classname="pkg.Test" name="runs"/></testsuite>`)
	if _, err := Parse(JUnitMediaType, data, []string{"pkg.Test#runs", "pkg.Test#runs"}, 1024); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("duplicate expected identity error = %v", err)
	}
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs = %#v, want %#v", got, want)
	}
}
